// Package media: turbojpeg.go provides CGO bindings to libjpeg-turbo for
// fast JPEG encode/decode. The Go standard library's image/jpeg is too slow
// for 1080p@120fps screen sharing (15-30ms/encode, 10-20ms/decode);
// libjpeg-turbo with SIMD acceleration brings this down to 1-3ms.
//
// This file is only compiled when CGO_ENABLED=1. The static libturbojpeg.a
// and turbojpeg.h live in ./libjpeg-turbo/ (shipped with the repo).
package media

/*
#cgo CFLAGS: -I${SRCDIR}/libjpeg-turbo
#cgo LDFLAGS: -L${SRCDIR}/libjpeg-turbo -lturbojpeg -lwinpthread -lm

#include <stdlib.h>
#include <string.h>
#include <stdio.h>
#include "libjpeg-turbo/turbojpeg.h"

// ── __iob_func shim ──────────────────────────────────────────────────────
// The prebuilt libturbojpeg.a was compiled against the classic MSVCRT
// runtime, which exports __iob_func (returning FILE* to the {stdin,stdout,
// stderr} array). Our MinGW UCRT build provides __acrt_iob_func instead, so
// the linker reports `undefined reference to __imp___iob_func` (the dllimport
// pointer for __iob_func). libjpeg-turbo only uses these streams for printing
// error/warning messages via fprintf, which we never rely on (we use
// tjGetErrorStr/tjGetErrorCode). We define a stub __iob_func that returns a
// pointer to a small static FILE array initialized from the real UCRT
// streams, plus the __imp___iob_func pointer the .a references.
// (stdio.h declares __acrt_iob_func; we just call it.)

static FILE _ljt_iob[3];
static int _ljt_iob_done = 0;

static void _ljt_iob_init(void) {
    if (_ljt_iob_done) return;
    _ljt_iob_done = 1;
    for (unsigned i = 0; i < 3; i++) {
        FILE *f = __acrt_iob_func(i);
        if (f) _ljt_iob[i] = *f;  // shallow copy; fprintf writes won't flush, which is fine
    }
}

FILE * __iob_func(void) {
    _ljt_iob_init();
    return _ljt_iob;
}
// __imp___iob_func is the dllimport pointer the prebuilt .a references for
// indirect calls to __iob_func. Point it at our shim.
FILE * (*__imp___iob_func)(void) = &__iob_func;

// Helper: encode a packed-pixel buffer to JPEG. pixelFormat is one of the
// TJPF_* constants (TJPF_RGBX for Go image.RGBA, TJPF_BGRA for WGC capture).
// Returns malloc'd buffer (caller frees via tjFree) and writes size to
// *outSize. Returns NULL on error.
static unsigned char* tj_encode_pixels(int width, int pitch, int height,
                                       const unsigned char* src,
                                       unsigned long* outSize,
                                       int pixelFormat, int quality) {
    tjhandle h = tjInitCompress();
    if (h == 0) return NULL;
    unsigned char* dst = NULL;
    // TJSAMP_420 + TJFLAG_FASTDCT for speed/size balance.
    int rc = tjCompress2(h, src, width, pitch, height, pixelFormat,
                         &dst, outSize, TJSAMP_420, quality,
                         TJFLAG_FASTDCT);
    tjDestroy(h);
    if (rc != 0) {
        if (dst) tjFree(dst);
        return NULL;
    }
    return dst;
}

// Helper: decode a JPEG into a freshly malloc'd RGBA buffer (top-down, pitch
// = width*4). Caller frees with free(). Returns NULL on error; writes width
// and height to *outW/*outH.
static unsigned char* tj_decode_rgba(const unsigned char* src,
                                     unsigned long size,
                                     int* outW, int* outH) {
    tjhandle h = tjInitDecompress();
    if (h == 0) return NULL;
    int w = 0, h2 = 0, subsamp = 0, cs = 0;
    if (tjDecompressHeader3(h, src, size, &w, &h2, &subsamp, &cs) != 0) {
        tjDestroy(h);
        return NULL;
    }
    int pitch = w * 4;
    unsigned char* dst = (unsigned char*)malloc((size_t)pitch * (size_t)h2);
    if (dst == NULL) {
        tjDestroy(h);
        return NULL;
    }
    // TJPF_RGBA: write R,G,B and alpha=0xFF (opaque) — matches Fyne canvas.
    if (tjDecompress2(h, src, size, dst, w, pitch, h2, TJPF_RGBA,
                      TJFLAG_FASTDCT) != 0) {
        free(dst);
        tjDestroy(h);
        return NULL;
    }
    tjDestroy(h);
    *outW = w;
    *outH = h2;
    return dst;
}
*/
import "C"

import (
	"errors"
	"image"
	"unsafe"
)

// turboEncodePixels is the shared implementation: encode a raw packed-pixel
// buffer (4 bytes/pixel, top-down) with the given TJPF pixel format.
func turboEncodePixels(data []byte, width, height, pitch, pixelFormat, quality int) ([]byte, error) {
	if width <= 0 || height <= 0 {
		return nil, errors.New("turbojpeg: empty image")
	}
	if pitch < width*4 {
		return nil, errors.New("turbojpeg: stride too small")
	}
	if len(data) < pitch*height {
		return nil, errors.New("turbojpeg: buffer too small")
	}

	var outSize C.ulong
	dst := C.tj_encode_pixels(C.int(width), C.int(pitch), C.int(height),
		(*C.uchar)(unsafe.Pointer(&data[0])), &outSize, C.int(pixelFormat), C.int(quality))
	if dst == nil {
		return nil, errors.New("turbojpeg: tjCompress2 failed")
	}
	defer C.tjFree(dst)

	// Copy into a Go-managed slice. tjCompress2 returns a buffer allocated by
	// libjpeg-turbo (via malloc inside tjCompress2); we must copy before free.
	n := int(outSize)
	out := make([]byte, n)
	C.memcpy(unsafe.Pointer(&out[0]), unsafe.Pointer(dst), C.size_t(n))
	return out, nil
}

// TJPF_RGBX / TJPF_BGRA constants mirror the C enum values in turbojpeg.h.
const (
	tjpfRGBX = 2 // TJPF_RGBX: R,G,B,X (matches Go image.RGBA)
	tjpfBGRA = 8 // TJPF_BGRA: B,G,R,A (matches WGC capture output)
)

// TurboEncode compresses an RGBA image to JPEG using libjpeg-turbo. The input
// must be *image.RGBA (top-down, 4 bytes/pixel). quality is 1-100.
func TurboEncode(rgba *image.RGBA, quality int) ([]byte, error) {
	if rgba == nil {
		return nil, errors.New("turbojpeg: nil image")
	}
	bounds := rgba.Bounds()
	w := bounds.Dx()
	h := bounds.Dy()
	return turboEncodePixels(rgba.Pix, w, h, rgba.Stride, tjpfRGBX, quality)
}

// TurboEncodeBGRA compresses a raw BGRA buffer (top-down, 4 bytes/pixel) to
// JPEG. This lets the WGC capture path avoid a BGRA→RGBA channel-swap copy.
// pitch is the byte stride of one row (usually width*4).
func TurboEncodeBGRA(bgra []byte, width, height, pitch, quality int) ([]byte, error) {
	return turboEncodePixels(bgra, width, height, pitch, tjpfBGRA, quality)
}

// TurboDecode decompresses JPEG bytes into an *image.RGBA (top-down, opaque
// alpha). Returns the image and its bounds.
func TurboDecode(data []byte) (image.Image, error) {
	if len(data) == 0 {
		return nil, errors.New("turbojpeg: empty input")
	}

	var outW, outH C.int
	dst := C.tj_decode_rgba(
		(*C.uchar)(unsafe.Pointer(&data[0])),
		C.ulong(len(data)), &outW, &outH)
	if dst == nil {
		return nil, errors.New("turbojpeg: tjDecompress2 failed")
	}
	defer C.free(unsafe.Pointer(dst))

	w := int(outW)
	h := int(outH)
	if w <= 0 || h <= 0 {
		return nil, errors.New("turbojpeg: bad dimensions")
	}

	// Build a Go image.RGBA and copy the decoded pixels in. We copy because
	// the C buffer is malloc'd and must be freed before returning.
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	pitch := w * 4
	C.memcpy(unsafe.Pointer(&rgba.Pix[0]), unsafe.Pointer(dst), C.size_t(pitch*h))
	return rgba, nil
}
