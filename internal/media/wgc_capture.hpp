#pragma once

#include <stdint.h>
#include <windows.h>

// C interface for WGC (Windows Graphics Capture) screen capture.
// Implemented in wgc_capture.cpp using WinRT + Direct3D11.

#ifdef __cplusplus
extern "C" {
#endif

// Opaque handle to a WGC capture session.
typedef struct WgcCapture WgcCapture;

// Returns 1 if WGC is supported on this system (Windows 10 1809+), 0 otherwise.
int wgc_is_supported(void);

// Creates a WGC capture session for the given display index.
// Returns NULL on failure.
// frame_callback receives mapped BGRA rows with the given stride; copy before returning.
typedef void (*wgc_frame_cb)(const uint8_t* bgra, int width, int height, int stride, int64_t age_ns, void* userdata);
WgcCapture* wgc_create(int display_index, wgc_frame_cb callback, void* userdata);

// Starts the capture session. Returns 0 on success.
int wgc_start(WgcCapture* cap, int fps);

// Stops the capture session.
void wgc_stop(WgcCapture* cap);

// Releases all resources.
void wgc_destroy(WgcCapture* cap);

#ifdef __cplusplus
}
#endif
