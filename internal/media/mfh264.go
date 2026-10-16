// mfh264.go - Go bindings for the Media Foundation H.264 wrapper
// (mf_h264.cpp). Windows-only: the relay client is a native Windows desktop
// app, and MF ships with the OS (no third-party DLLs).
package media

/*
#cgo LDFLAGS: -lmfuuid -lmfplat -lmf -lstrmiids -lole32 -loleaut32
#include <stdlib.h>
#include "mf_h264.hpp"

extern void h264GoLogTrampoline(char* msg);

// C helper: installs the Go trampoline as the MF log callback (casting the
// function pointer types here keeps the Go side clean).
static void h264InstallGoLogCallback(void) {
    h264_set_log_callback((h264_log_cb)h264GoLogTrampoline);
}
*/
import "C"

import (
	"errors"
	"unsafe"
)

// SetH264LogCallback routes Media Foundation diagnostics into the app's UI
// log (the client is a -H windowsgui binary, stderr is invisible). Safe to
// call once at startup; pass nil to clear.
func SetH264LogCallback(fn func(string)) {
	if fn == nil {
		C.h264_set_log_callback(nil)
		return
	}
	h264GoLogFn = fn
	C.h264InstallGoLogCallback()
}

var h264GoLogFn func(string)

//export h264GoLogTrampoline
func h264GoLogTrampoline(msg *C.char) {
	if h264GoLogFn != nil {
		h264GoLogFn(C.GoString(msg))
	}
}

// H264Encoder wraps a Media Foundation H.264 encoder. Hardware encoder is
// preferred at creation time; the Microsoft software encoder is the fallback.
type H264Encoder struct {
	p *C.h264_encoder
}

func (e *H264Encoder) IsHardware() bool {
	return e.p != nil && C.h264_enc_is_hardware(e.p) != 0
}

// NewH264Encoder creates an encoder that scales frames from inW×inH down to
// outW×outH (nearest-neighbor, during BGRA→NV12 conversion) and encodes them
// as H.264. fps sets the nominal rate and GOP size (keyframe ~1×/second);
// bitrateKbps is the target average bitrate.
func NewH264Encoder(inW, inH, outW, outH, fps, bitrateKbps int) (*H264Encoder, error) {
	e := C.h264_enc_create(C.int(inW), C.int(inH), C.int(outW), C.int(outH),
		C.int(fps), C.int(bitrateKbps))
	if e == nil {
		return nil, errors.New("mf: H264 encoder unavailable")
	}
	return &H264Encoder{p: e}, nil
}

// Send encodes one top-down 4-byte-per-pixel frame. swapRB selects the input
// channel order: false = BGRA (WGC capture), true = RGBA (Go image.RGBA).
// Returns the Annex-B bitstream for this frame and whether it contains an IDR
// keyframe. A non-nil empty bitstream means the encoder dropped the frame
// (busy) — the caller should simply skip it and keep streaming.
func (e *H264Encoder) Send(pixels []byte, stride int, swapRB bool) ([]byte, bool, error) {
	data, key, _, err := e.SendAt(pixels, stride, swapRB, CaptureTimestamp())
	return data, key, err
}

func (e *H264Encoder) SendAt(pixels []byte, stride int, swapRB bool, timestampNS int64) (data []byte, keyframe bool, outputPTS int64, err error) {
	if e.p == nil || len(pixels) == 0 || stride <= 0 {
		return nil, false, 0, errors.New("mf: invalid encoder input")
	}
	var outLen, key C.int
	var pts C.int64_t
	var cp *C.uchar
	rc := C.h264_enc_send(e.p, (*C.uchar)(unsafe.Pointer(&pixels[0])), C.int(stride), boolToCInt(swapRB), C.int64_t(timestampNS), &cp, &outLen, &key, &pts)
	if rc != 0 {
		return nil, false, 0, errors.New("mf: encode failed")
	}
	if cp != nil && outLen > 0 {
		data = C.GoBytes(unsafe.Pointer(cp), outLen)
		C.h264_buf_free(unsafe.Pointer(cp))
	}
	return data, key != 0, int64(pts), nil
}

// SubmitAt and Poll are used together on one encoder thread. Busy returns
// false without encoding that raw picture; accepted references must be polled.
func (e *H264Encoder) SubmitAt(pixels []byte, stride int, swapRB bool, timestampNS int64) (bool, error) {
	if e.p == nil || len(pixels) == 0 || stride <= 0 {
		return false, errors.New("mf: invalid encoder input")
	}
	rc := C.h264_enc_submit(e.p, (*C.uchar)(unsafe.Pointer(&pixels[0])), C.int(stride), boolToCInt(swapRB), C.int64_t(timestampNS))
	if rc < 0 {
		return false, errors.New("mf: pipeline input failed")
	}
	return rc == 0, nil
}

func (e *H264Encoder) Poll() ([]byte, bool, int64, error) {
	if e.p == nil {
		return nil, false, 0, errors.New("mf: encoder closed")
	}
	var cp *C.uchar
	var length, key C.int
	var pts C.int64_t
	if C.h264_enc_poll(e.p, &cp, &length, &key, &pts) != 0 {
		return nil, false, 0, errors.New("mf: pipeline output failed")
	}
	var data []byte
	if cp != nil {
		data = C.GoBytes(unsafe.Pointer(cp), length)
		C.h264_buf_free(unsafe.Pointer(cp))
	}
	return data, key != 0, int64(pts), nil
}

// Close releases the underlying MFT.
func (e *H264Encoder) Close() {
	if e.p != nil {
		C.h264_enc_destroy(e.p)
		e.p = nil
	}
}

// H264Decoder wraps a Media Foundation H.264 decoder (hardware preferred).
type H264Decoder struct {
	p *C.h264_decoder
}

// NewH264Decoder creates a decoder. Output frames are delivered as top-down
// RGBA (NV12→RGBA conversion happens inside the C++ layer).
func NewH264Decoder() (*H264Decoder, error) {
	d := C.h264_dec_create()
	if d == nil {
		return nil, errors.New("mf: H264 decoder unavailable")
	}
	return &H264Decoder{p: d}, nil
}

// Send feeds one frame worth of Annex-B NAL bytes and returns the decoded
// image (RGBA byte layout + dimensions). A nil image with nil error means the
// decoder produced nothing this round (normal while it buffers, or when a
// P-frame arrives before the first keyframe). A non-nil error marks the
// stream as broken (e.g. UDP loss broke the reference chain) — the caller
// should Close() and recreate the decoder, then wait for the next keyframe.
func (d *H264Decoder) Send(nal []byte) ([]byte, int, int, error) {
	data, w, h, _, err := d.SendAt(nal, CaptureTimestamp())
	return data, w, h, err
}

func (d *H264Decoder) SendAt(nal []byte, timestampNS int64) (rgba []byte, w, h int, outputPTS int64, err error) {
	return d.SendSizedAt(nal, timestampNS, 0, 0)
}

// SendSizedAt converts only the pixels needed by the display, preserving
// aspect ratio. Zero limits keep the original resolution; limits never upscale.
func (d *H264Decoder) SendSizedAt(nal []byte, timestampNS int64, maxW, maxH int) (rgba []byte, w, h int, outputPTS int64, err error) {
	if d.p == nil {
		return nil, 0, 0, 0, errors.New("mf: decoder closed")
	}
	if len(nal) == 0 {
		return nil, 0, 0, 0, nil
	}
	var cp *C.uchar
	var cw, ch C.int
	var pts C.int64_t
	rc := C.h264_dec_feed(d.p, (*C.uchar)(unsafe.Pointer(&nal[0])), C.int(len(nal)), C.int64_t(timestampNS), C.int(maxW), C.int(maxH), &cp, &cw, &ch, &pts)
	if rc != 0 {
		return nil, 0, 0, 0, errors.New("mf: H264 stream broken")
	}
	if cp != nil && cw > 0 && ch > 0 {
		rgba = C.GoBytes(unsafe.Pointer(cp), C.int(cw*ch*4))
		C.h264_buf_free(unsafe.Pointer(cp))
	}
	return rgba, int(cw), int(ch), int64(pts), nil
}

// SkipAt consumes expired reference frames without converting them into
// large display images. It preserves the decoder state for later P-frames.
func (d *H264Decoder) SkipAt(nal []byte, timestampNS int64) error {
	if d.p == nil {
		return errors.New("mf: decoder closed")
	}
	if len(nal) == 0 {
		return nil
	}
	if C.h264_dec_skip(d.p, (*C.uchar)(unsafe.Pointer(&nal[0])), C.int(len(nal)), C.int64_t(timestampNS)) != 0 {
		return errors.New("mf: H264 stream broken")
	}
	return nil
}

// Close releases the underlying MFT.
func (d *H264Decoder) Close() {
	if d.p != nil {
		C.h264_dec_destroy(d.p)
		d.p = nil
	}
}

func boolToCInt(b bool) C.int {
	if b {
		return 1
	}
	return 0
}
