package media

/*
#cgo LDFLAGS: -ld3d11 -ldxgi -lwindowsapp -ldwmapi
#cgo CXXFLAGS: -std=c++17 -O3
#include "wgc_capture.hpp"
#include <objbase.h>

extern void goWgcFrameCallback(uint8_t* bgra, int width, int height, int stride, int64_t age_ns, void* userdata);
*/
import "C"

import (
	"errors"
	"fmt"
	"runtime"
	"sync"
	"time"
	"unsafe"
)

var (
	errWGCCreateFailed = errors.New("wgc: failed to create capture session")
	errWGCStartFailed  = errors.New("wgc: failed to start capture")
)

// CaptureMethod selects the underlying screen-capture technology.
type CaptureMethod int

const (
	// CaptureMethodAuto picks WGC on Windows 10 1809+ and DXGI otherwise.
	CaptureMethodAuto CaptureMethod = iota
	// CaptureMethodWGC forces Windows Graphics Capture.
	CaptureMethodWGC
	// CaptureMethodDXGI forces DXGI Desktop Duplication (BitBlt fallback).
	CaptureMethodDXGI
)

// CaptureMethodNames maps methods to their display labels (index == method).
var CaptureMethodNames = []string{"自动", "WGC", "DXGI"}

// IsWGCSupported reports whether Windows Graphics Capture is available on
// this system (Windows 10 1903 / build 18362+ for CreateFreeThreaded API).
func IsWGCSupported() bool {
	return C.wgc_is_supported() != 0
}

// ResolveMethod returns the concrete method to use: for Auto it picks WGC when
// supported, otherwise DXGI. Explicit WGC/DXGI are returned unchanged.
func ResolveMethod(m CaptureMethod) CaptureMethod {
	if m == CaptureMethodAuto {
		if IsWGCSupported() {
			return CaptureMethodWGC
		}
		return CaptureMethodDXGI
	}
	return m
}

// WgcCapturer wraps the C++ WGC capture implementation. Frames are delivered
// to the configured callback, H.264-encoded when Media Foundation is
// available (JPEG otherwise).
type WgcCapturer struct {
	cap          *C.WgcCapture
	displayIndex int
	callback     func(VideoFrame)
	quality      VideoQuality

	mu          sync.Mutex
	handle      int
	handlePtr   *C.int // C heap memory holding the handle (passed as userdata)
	running     bool
	h264        *H264Encoder // created lazily on first frame (native size known then)
	h264W       int          // capture width the encoder was created for
	h264H       int          // capture height the encoder was created for
	h264Fail    bool         // encoder creation previously failed: stay on JPEG
	lastFrameNS int64
	captured    bool
	rawFrames   chan rawCaptureFrame
	rawPool     sync.Pool
	encoderStop chan struct{}
	encoderDone chan struct{}
	outputs     map[*WgcCapturer]struct{} // extra encoders sharing this native capture
}

type rawCaptureFrame struct {
	pixels        []byte
	width, height int
	timestampNS   int64
}

var (
	wgcMu         sync.Mutex
	wgcHandles    = make(map[int]*WgcCapturer)
	wgcNextHandle int
)

func registerWgc(w *WgcCapturer) int {
	wgcMu.Lock()
	defer wgcMu.Unlock()
	wgcNextHandle++
	h := wgcNextHandle
	wgcHandles[h] = w
	return h
}

func unregisterWgc(h int) {
	wgcMu.Lock()
	defer wgcMu.Unlock()
	delete(wgcHandles, h)
}

func getWgc(h int) *WgcCapturer {
	wgcMu.Lock()
	defer wgcMu.Unlock()
	return wgcHandles[h]
}

// NewWgcCapturer creates a WGC capture session for the given display.
// Returns nil if WGC is not supported.
func NewWgcCapturer(displayIndex int, quality VideoQuality, callback func(VideoFrame)) *WgcCapturer {
	if !IsWGCSupported() {
		return nil
	}
	w := &WgcCapturer{
		displayIndex: displayIndex,
		callback:     callback,
		quality:      quality,
	}
	return w
}

// Start begins capture. Returns an error if the session could not start.
func (w *WgcCapturer) Start() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running {
		return nil
	}

	w.handle = registerWgc(w)
	// Store the integer handle in C heap memory so we can pass it as void*
	// without triggering the uintptr->unsafe.Pointer vet warning.
	w.handlePtr = (*C.int)(C.malloc(C.sizeof_int))
	*w.handlePtr = C.int(w.handle)
	userdata := unsafe.Pointer(w.handlePtr)

	cap := C.wgc_create(C.int(w.displayIndex), (C.wgc_frame_cb)(C.goWgcFrameCallback), userdata)
	if cap == nil {
		C.free(unsafe.Pointer(w.handlePtr))
		w.handlePtr = nil
		unregisterWgc(w.handle)
		w.handle = 0
		return errWGCCreateFailed
	}
	w.cap = cap

	if rc := C.wgc_start(cap, C.int(w.quality.FPS)); rc != 0 {
		C.wgc_destroy(cap)
		w.cap = nil
		C.free(unsafe.Pointer(w.handlePtr))
		w.handlePtr = nil
		unregisterWgc(w.handle)
		w.handle = 0
		return errWGCStartFailed
	}

	w.running = true
	w.captured = false
	w.rawFrames = make(chan rawCaptureFrame, 1)
	w.encoderStop = make(chan struct{})
	w.encoderDone = make(chan struct{})
	go w.encodeLoop()
	return nil
}

// Stop halts capture and releases the native session.
func (w *WgcCapturer) Stop() {
	w.mu.Lock()
	if !w.running {
		done := w.encoderDone
		w.mu.Unlock()
		if done != nil {
			<-done
		}
		return
	}
	w.running = false
	cap := w.cap
	w.mu.Unlock()

	// Join capture before stopping the encoder worker and releasing its buffers.
	if cap != nil {
		C.wgc_destroy(cap)
	}
	close(w.encoderStop)
	<-w.encoderDone
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cap = nil
	if w.handlePtr != nil {
		C.free(unsafe.Pointer(w.handlePtr))
		w.handlePtr = nil
	}
	if w.handle != 0 {
		unregisterWgc(w.handle)
		w.handle = 0
	}
}

func (w *WgcCapturer) AddOutput(q VideoQuality, callback func(VideoFrame)) func() {
	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return func() {}
	}
	output := &WgcCapturer{quality: q, callback: callback, running: true,
		rawFrames: make(chan rawCaptureFrame, 1), encoderStop: make(chan struct{}), encoderDone: make(chan struct{})}
	if w.outputs == nil {
		w.outputs = make(map[*WgcCapturer]struct{})
	}
	w.outputs[output] = struct{}{}
	go output.encodeLoop()
	w.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			w.mu.Lock()
			delete(w.outputs, output)
			w.mu.Unlock()
			close(output.encoderStop)
			<-output.encoderDone
		})
	}
}

//export goWgcFrameCallback
func goWgcFrameCallback(bgra *C.uint8_t, width C.int, height C.int, stride C.int, ageNS C.int64_t, userdata unsafe.Pointer) {
	handle := int(*(*C.int)(userdata))
	w := getWgc(handle)
	if w == nil || w.callback == nil {
		return
	}

	wCap := int(width)
	hCap := int(height)
	if wCap <= 0 || hCap <= 0 {
		return
	}

	w.mu.Lock()
	if !w.running {
		w.mu.Unlock()
		return
	}
	capturePTS := max(CaptureTimestamp()-int64(ageNS), 0)
	if !w.acceptCapture(capturePTS) {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	// Native mapped memory expires as soon as this callback returns. Copy it
	// once into reusable owned storage; encoding happens on another thread.
	pitch := wCap * 4
	var pixels []byte
	if pooled := w.rawPool.Get(); pooled != nil {
		pixels = pooled.([]byte)
	}
	if cap(pixels) < pitch*hCap {
		pixels = make([]byte, pitch*hCap)
	}
	pixels = pixels[:pitch*hCap]
	source := unsafe.Slice((*byte)(unsafe.Pointer(bgra)), int(stride)*hCap)
	for row := 0; row < hCap; row++ {
		copy(pixels[row*pitch:(row+1)*pitch], source[row*int(stride):row*int(stride)+pitch])
	}
	// Copy before the primary worker can return pixels to its pool. Each
	// encoder owns its raw storage; only unencoded pictures may be replaced.
	w.mu.Lock()
	for output := range w.outputs {
		if !output.acceptCapture(capturePTS) {
			continue
		}
		var owned []byte
		if pooled := output.rawPool.Get(); pooled != nil {
			owned = pooled.([]byte)
		}
		if cap(owned) < len(pixels) {
			owned = make([]byte, len(pixels))
		}
		owned = owned[:len(pixels)]
		copy(owned, pixels)
		output.queueRaw(rawCaptureFrame{pixels: owned, width: wCap, height: hCap, timestampNS: capturePTS})
	}
	w.mu.Unlock()
	w.queueRaw(rawCaptureFrame{pixels: pixels, width: wCap, height: hCap, timestampNS: capturePTS})
}

// Called under mu: readback timing may jitter, but picture timestamps retain
// the desktop cadence and the common audio/video clock.
func (w *WgcCapturer) acceptCapture(timestampNS int64) bool {
	if w.captured && (timestampNS <= w.lastFrameNS ||
		(w.quality.FPS > 0 && timestampNS-w.lastFrameNS < int64(time.Second)/int64(w.quality.FPS)*9/10)) {
		return false
	}
	w.lastFrameNS, w.captured = timestampNS, true
	return true
}

func (w *WgcCapturer) queueRaw(frame rawCaptureFrame) {
	select {
	case w.rawFrames <- frame:
		return
	default:
	}
	select {
	case old := <-w.rawFrames:
		w.rawPool.Put(old.pixels)
	default:
	}
	w.rawFrames <- frame // one producer; a consumer can only free this slot
}

func (w *WgcCapturer) emitH264(data []byte, keyframe bool, pts int64) {
	typ := FrameTypeH264P
	if keyframe {
		typ = FrameTypeH264IDR
	}
	w.callback(VideoFrame{Data: data, Type: typ, TimestampNS: pts})
}

// Native diagnostics must query the current OS thread, not the capture caller.
func currentMediaApartment() (int, int) {
	var apartment C.APTTYPE
	var qualifier C.APTTYPEQUALIFIER
	if C.CoGetApartmentType(&apartment, &qualifier) < 0 {
		return -1, -1
	}
	return int(apartment), int(qualifier)
}

func (w *WgcCapturer) pollEncoder() error {
	if w.h264 == nil || !w.h264.IsHardware() {
		return nil
	}
	for {
		data, keyframe, pts, err := w.h264.Poll()
		if err != nil {
			return err
		}
		if len(data) == 0 {
			return nil
		}
		w.emitH264(data, keyframe, pts)
	}
}

func (w *WgcCapturer) encodeLoop() {
	defer close(w.encoderDone)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if C.CoInitializeEx(nil, C.COINIT_MULTITHREADED) < 0 {
		fmt.Println("h264: WGC encoder thread could not initialize COM, JPEG fallback")
		w.h264Fail = true
	} else {
		defer C.CoUninitialize()
	}
	var pending *rawCaptureFrame
	defer func() {
		if pending != nil {
			w.rawPool.Put(pending.pixels)
		}
		if w.h264 != nil {
			w.h264.Close()
			w.h264 = nil
		}
	}()
	// Poll outputs even when the desktop stops changing, so the last submitted
	// picture is delivered without requiring another capture event.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-w.encoderStop:
			return
		case raw := <-w.rawFrames:
			if pending != nil {
				w.rawPool.Put(pending.pixels)
			}
			pending = &raw
		case <-ticker.C:
		}
		if err := w.pollEncoder(); err != nil {
			fmt.Printf("h264: WGC pipeline failed (%v), JPEG fallback\n", err)
			w.h264.Close()
			w.h264 = nil
			w.h264Fail = true
		}
		if pending == nil {
			continue
		}
		if w.encodeRaw(*pending) {
			w.rawPool.Put(pending.pixels)
			pending = nil
		}
	}
}

// Returns false only if the raw picture has not entered the busy encoder.
func (w *WgcCapturer) encodeRaw(frame rawCaptureFrame) bool {
	wCap, hCap := frame.width, frame.height
	bgraSlice, pitch, capturePTS := frame.pixels, frame.width*4, frame.timestampNS
	enc := w.h264
	if enc == nil && !w.h264Fail {
		var cerr error
		enc, cerr = NewH264Encoder(wCap, hCap, w.quality.Width, w.quality.Height,
			w.quality.FPS, w.quality.Bitrate)
		if cerr != nil {
			fmt.Printf("h264: WGC encoder unavailable (%v), JPEG fallback\n", cerr)
			w.h264Fail = true
		} else {
			w.h264 = enc
			w.h264W = wCap
			w.h264H = hCap
		}
	} else if enc != nil && (w.h264W != wCap || w.h264H != hCap) {
		// Capture resolution changed (monitor swap): rebuild the encoder.
		enc.Close()
		enc, _ = NewH264Encoder(wCap, hCap, w.quality.Width, w.quality.Height,
			w.quality.FPS, w.quality.Bitrate)
		if enc == nil {
			fmt.Printf("h264: WGC encoder rebuild failed, JPEG fallback\n")
			w.h264 = nil
			w.h264Fail = true
		} else {
			w.h264 = enc
			w.h264W = wCap
			w.h264H = hCap
		}
	}

	if enc != nil {
		if enc.IsHardware() {
			accepted, err := enc.SubmitAt(bgraSlice, pitch, false, capturePTS)
			if err == nil {
				return accepted
			}
			fmt.Printf("h264: WGC pipeline input failed (%v), JPEG fallback\n", err)
			enc.Close()
			w.h264 = nil
			w.h264Fail = true
			enc = nil
		}
	}
	if enc != nil {
		data, keyframe, outputPTS, err := enc.SendAt(bgraSlice, pitch, false, capturePTS)
		if err != nil {
			// Encoder broken: disable and fall back to JPEG permanently.
			fmt.Printf("h264: WGC encode failed (%v), JPEG fallback\n", err)
			enc.Close()
			if w.h264 == enc {
				w.h264 = nil
				w.h264Fail = true
			}
			enc = nil
		} else if len(data) > 0 {
			typ := FrameTypeH264P
			if keyframe {
				typ = FrameTypeH264IDR
			}
			w.callback(VideoFrame{Data: data, Type: typ, TimestampNS: outputPTS})
			return true
		}
		// Empty bitstream = encoder dropped the frame; skip (don't re-encode
		// as JPEG — duplicate work for a frame we already decided to drop).
		if enc != nil {
			return true
		}
	}

	// JPEG fallback path.
	targetW, targetH := w.quality.Width, w.quality.Height
	var encoded []byte
	var err error
	if wCap == targetW && hCap == targetH {
		// Capture matches target: encode the BGRA buffer directly with
		// libjpeg-turbo — no copy, no scaling.
		encoded, err = TurboEncodeBGRA(bgraSlice, wCap, hCap, pitch, 80)
	} else {
		// Single-pass BGRA→RGBA nearest-neighbor scale, then turbo-encode.
		scaled := scaleBGRAtoRGBA(bgraSlice, wCap, hCap, pitch, targetW, targetH)
		encoded, err = TurboEncode(scaled, 80)
	}
	if err != nil {
		return true
	}
	w.callback(VideoFrame{Data: encoded, Type: FrameTypeJPEG, TimestampNS: capturePTS})
	return true
}
