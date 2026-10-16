// mf_h264.hpp - C API for the Media Foundation H.264 encoder/decoder wrapper.
// Windows-only; consumed from Go via CGO (see mfh264.go).
#ifndef MF_H264_HPP
#define MF_H264_HPP
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

typedef struct h264_encoder h264_encoder;
typedef struct h264_decoder h264_decoder;

// Optional log callback so diagnostics reach the app UI (stderr is invisible
// in a -H windowsgui build). Pass NULL to clear.
typedef void (*h264_log_cb)(const char* msg);
void h264_set_log_callback(h264_log_cb cb);

// ── Encoder ──────────────────────────────────────────────────────────────
// Creates an H.264 encoder that converts BGRA/RGBA frames of in_w x in_h to
// NV12 at out_w x out_h (nearest-neighbor, done inside feed) and encodes
// them. fps sets the nominal rate and GOP; input capture timestamps are preserved.
// Hardware encoder (MFTEnumEx HARDWARE) preferred, software fallback.
// Returns NULL when Media Foundation has no usable encoder.
h264_encoder* h264_enc_create(int in_w, int in_h, int out_w, int out_h,
                              int fps, int bitrate_kbps);

// Encodes one top-down 4-byte frame (stride = bytes/row).
//   swap_rb: 0 = BGRA input (WGC), 1 = RGBA input (Go image.RGBA).
// On return: *out_data points to a malloc'd Annex-B bitstream for this frame
// (free with h264_buf_free), *out_len is its length, *out_keyframe = 1 if it
// contains an IDR NAL. Returns 0 on success. When the encoder is busy/dropped
// the frame, returns 0 with *out_len == 0 (caller skips the frame). Returns
// -1 on hard failure.
int h264_enc_send(h264_encoder* e, const unsigned char* pixels, int stride,
                  int swap_rb, int64_t timestamp_ns, unsigned char** out_data, int* out_len,
                  int* out_keyframe, int64_t* output_timestamp_ns);

void h264_enc_destroy(h264_encoder* e);
int h264_enc_is_hardware(h264_encoder* e);

// Nonblocking hardware pipeline: submit returns 1 when busy (input not
// accepted), 0 when accepted, -1 on failure. Poll returns one access unit
// with its original capture timestamp; never merges distinct pictures.
int h264_enc_submit(h264_encoder* e, const unsigned char* pixels, int stride,
                    int swap_rb, int64_t timestamp_ns);
int h264_enc_poll(h264_encoder* e, unsigned char** out_data, int* out_len,
                  int* out_keyframe, int64_t* output_timestamp_ns);

// ── Decoder ──────────────────────────────────────────────────────────────
// Creates an H.264 decoder (hardware preferred, software fallback).
// Returns NULL when unavailable.
h264_decoder* h264_dec_create(void);

// Feeds one frame's Annex-B bitstream. On success *out_rgba points to a
// malloc'd top-down RGBA buffer (w x h, free with h264_buf_free); when the
// decoder has not produced a frame yet (e.g. P-frame fed before any IDR /
// needs more data), returns 0 with *out_rgba == NULL. Returns -1 if the
// decoder rejected the data (stream broken) — caller should recreate and
// wait for the next keyframe.
int h264_dec_feed(h264_decoder* d, const unsigned char* data, int len, int64_t timestamp_ns,
                  int max_w, int max_h,
                  unsigned char** out_rgba, int* out_w, int* out_h, int64_t* output_timestamp_ns);

// Consume a reference frame without allocating a display image.
int h264_dec_skip(h264_decoder* d, const unsigned char* data, int len, int64_t timestamp_ns);

void h264_dec_destroy(h264_decoder* d);

// Frees buffers returned by the functions above.
void h264_buf_free(void* p);

#ifdef __cplusplus
}
#endif

#endif // MF_H264_HPP
