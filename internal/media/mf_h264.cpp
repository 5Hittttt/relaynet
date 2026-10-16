// mf_h264.cpp - Media Foundation H.264 encoder/decoder wrapper.
//
// Purpose: replace per-frame JPEG (intra-only, high bandwidth) with H.264
// inter-frame compression using the in-box Windows Media Foundation codecs
// (hardware-accelerated when the GPU vendor ships an MFT, otherwise the
// Microsoft software encoder/decoder). No third-party DLLs are introduced —
// mfplat.dll / mf.dll are Windows components.
//
// Design notes:
//  * Hardware inputs and outputs may be queued separately on one encoder
//    worker. LOW_LATENCY + no B-frames keep output prompt.
//  * Decoder is fed Annex-B access units and returns top-down RGBA so the
//    Go side can hand the image straight to Fyne.
//  * Color conversions (BGRA->NV12 with nearest-neighbor scale, NV12->RGBA)
//    are integer-only, single-pass.

#include "mf_h264.hpp"

#define WIN32_LEAN_AND_MEAN
#include <windows.h>
#include <oleauto.h>
#include <mfapi.h>
#include <mfidl.h>
#include <mftransform.h>
#include <mferror.h>
#include <strmif.h>   // ICodecAPI
#include <codecapi.h> // CODECAPI_* GUIDs
#include "frame_wait.hpp"

#include <cstdarg>
#include <chrono>
#include <cstdio>
#include <cstring>
#include <mutex>
#include <vector>

// ---------------------------------------------------------------------------
// Log callback: routes diagnostics to the Go UI log (a -H windowsgui binary
// has no visible stderr).
// ---------------------------------------------------------------------------
static std::mutex g_log_mutex;
static h264_log_cb g_log_cb = nullptr;

void h264_set_log_callback(h264_log_cb cb) {
    std::lock_guard<std::mutex> lock(g_log_mutex);
    g_log_cb = cb;
}

static void mf_log(const char* fmt, ...) {
    char buf[512];
    va_list ap;
    va_start(ap, fmt);
    vsnprintf(buf, sizeof(buf), fmt, ap);
    va_end(ap);
    h264_log_cb cb;
    {
        std::lock_guard<std::mutex> lock(g_log_mutex);
        cb = g_log_cb;
    }
    if (cb) cb(buf);
    fprintf(stderr, "[mf] %s\n", buf);
}

// ---------------------------------------------------------------------------
// Minimal COM pointer (ATL/WRL headers are avoided: MinGW's versions have
// template redefinition bugs, lesson learned with WGC).
// ---------------------------------------------------------------------------
template <typename T>
class ComPtr {
public:
    ComPtr() = default;
    explicit ComPtr(T* p) : p_(p) {}  // adopts
    ~ComPtr() { reset(); }
    ComPtr(const ComPtr&) = delete;
    ComPtr& operator=(const ComPtr&) = delete;

    T* get() const { return p_; }
    T* operator->() const { return p_; }
    explicit operator bool() const { return p_ != nullptr; }
    T** put() { reset(); return &p_; }
    void reset() {
        if (p_) { p_->Release(); p_ = nullptr; }
    }

private:
    T* p_ = nullptr;
};

// IIDs are declared locally so we never depend on __uuidof/link-time GUID
// symbols (MinGW import libs are finicky here).
static const IID kIID_ICodecAPI = {
    0x901db4c7, 0x31ce, 0x41a2, {0x85, 0xdc, 0x8f, 0xa0, 0xbf, 0x41, 0xb8, 0xda}};
static const IID kIID_IMFTransform = {
    0xbf94c121, 0x5b05, 0x4e6f, {0x80, 0x00, 0xba, 0x59, 0x89, 0x61, 0x41, 0x4d}};

static const GUID gMF_LOW_LATENCY = {
    0x9c27891a, 0xed7a, 0x40e1, {0x88, 0xe8, 0xb2, 0x27, 0x27, 0xa0, 0x24, 0xee}};

static const GUID gMF_TRANSFORM_ASYNC = {
    0xf81a699a, 0x649a, 0x497d, {0x8c, 0x73, 0x29, 0xf8, 0xfe, 0xd6, 0xad, 0x7a}};
static const GUID gMF_TRANSFORM_ASYNC_UNLOCK = {
    0xe5666d6b, 0x3422, 0x4eb6, {0xa4, 0x21, 0xda, 0x7d, 0xb1, 0xf8, 0xe2, 0x07}};
static const IID kIID_IMFMediaEventGenerator = {
    0x2cd0bd52, 0xbcd5, 0x4b89, {0xb6, 0x2c, 0xea, 0xdc, 0x0c, 0x03, 0x1e, 0x7d}};
static const IID kIID_IMFShutdown = {
    0x97ec2ea4, 0x0e42, 0x4937, {0x97, 0xac, 0x9d, 0x6d, 0x32, 0x88, 0x24, 0xe1}};

static const GUID gMF_MT_DEFAULT_STRIDE = {
    0x644b4e48, 0x1e02, 0x4516, {0xb0, 0xeb, 0xc0, 0x1c, 0xa9, 0xd4, 0x9a, 0xc6}};

// ---------------------------------------------------------------------------
// MFStartup once per process (thread-safe).
// ---------------------------------------------------------------------------
static std::mutex g_mf_mutex;
static bool g_mf_started = false;

static void ensure_mf() {
    std::lock_guard<std::mutex> lock(g_mf_mutex);
    if (!g_mf_started) {
        // MFSTARTUP_NOSOCKET: no Winsock init; still allows hardware MFTs.
        MFStartup(MF_VERSION, MFSTARTUP_NOSOCKET);
        g_mf_started = true;
    }
}

static uint8_t clamp8(int v) {
    if (v < 0) return 0;
    if (v > 255) return 255;
    return (uint8_t)v;
}

// ---------------------------------------------------------------------------
// BGRA/RGBA -> NV12 with nearest-neighbor scaling. Integer math, one pass.
// src: top-down, stride bytes/row, sw x sh; output sized dst_w x dst_h with
// y_stride (16-byte aligned) per row.
// ---------------------------------------------------------------------------
static void scale_to_nv12(const uint8_t* src, int sstride, int sw, int sh,
                          int dst_w, int dst_h, int y_stride, bool swap_rb,
                          uint8_t* nv12) {
    if (y_stride != dst_w) memset(nv12, 0, (size_t)y_stride * dst_h * 3 / 2);
    uint8_t* Y = nv12;
    uint8_t* UV = Y + (size_t)y_stride * dst_h;
    if (dst_w == sw * 2 && dst_h == sh * 2) {
        // A 1080p desktop shared at 4K repeats every source pixel. Compute
        // its colour once, retaining the same per-pixel chroma sampling.
        for (int sy = 0; sy < sh; sy++) {
            const uint8_t* srow = src + (size_t)sy * sstride;
            uint8_t* yrow = Y + (size_t)(sy * 2) * y_stride;
            uint8_t* uvrow = UV + (size_t)sy * y_stride;
            for (int sx = 0; sx < sw; sx++) {
                const uint8_t* p = srow + sx * 4;
                int b = p[0], g = p[1], r = p[2];
                if (swap_rb) { b = p[2]; r = p[0]; }
                uint8_t y = clamp8(((66 * r + 129 * g + 25 * b + 128) >> 8) + 16);
                yrow[sx * 2] = yrow[sx * 2 + 1] = y;
                uvrow[sx * 2] = clamp8(((-38 * r - 74 * g + 112 * b + 128) >> 8) + 128);
                uvrow[sx * 2 + 1] = clamp8(((112 * r - 94 * g - 18 * b + 128) >> 8) + 128);
            }
            memcpy(yrow + y_stride, yrow, dst_w);
        }
        return;
    }
    std::vector<int> x_offsets(dst_w);
    for (int x = 0; x < dst_w; x++) x_offsets[x] = (int)((int64_t)x * sw / dst_w) * 4;

    int previous_sy = -1;
    for (int dy = 0; dy < dst_h; dy++) {
        int sy = int(((long long)dy * sh) / dst_h);
        if (sy >= sh) sy = sh - 1;
        const uint8_t* srow = src + (size_t)sy * sstride;
        uint8_t* yrow = Y + (size_t)dy * y_stride;
        if (sy == previous_sy) {
            memcpy(yrow, yrow - y_stride, dst_w);
            continue;
        }
        previous_sy = sy;
        for (int dx = 0; dx < dst_w; dx++) {
            const uint8_t* p = srow + x_offsets[dx];
            int b = p[0], g = p[1], r = p[2];
            if (swap_rb) { b = p[2]; r = p[0]; }
            // BT.601 limited range
            yrow[dx] = clamp8(((66 * r + 129 * g + 25 * b + 128) >> 8) + 16);
        }
    }

    for (int dy = 0; dy < dst_h; dy += 2) {
        int sy = int(((long long)dy * sh) / dst_h);
        if (sy >= sh) sy = sh - 1;
        const uint8_t* srow = src + (size_t)sy * sstride;
        uint8_t* uvrow = UV + (size_t)(dy / 2) * y_stride;
        for (int dx = 0; dx < dst_w; dx += 2) {
            const uint8_t* p = srow + x_offsets[dx];
            int b = p[0], g = p[1], r = p[2];
            if (swap_rb) { b = p[2]; r = p[0]; }
            uvrow[dx] = clamp8(((-38 * r - 74 * g + 112 * b + 128) >> 8) + 128);
            uvrow[dx + 1] = clamp8(((112 * r - 94 * g - 18 * b + 128) >> 8) + 128);
        }
    }
}

// NV12 -> RGBA (top-down). nv12 stride passed explicitly (decoders may pad).
static void nv12_to_rgba(const uint8_t* nv12, int stride, int buffer_h,
                         int crop_x, int crop_y, int w, int h, int dst_w, int dst_h,
                         uint8_t* rgba) {
    const uint8_t* Y = nv12;
    const uint8_t* UV = nv12 + (size_t)stride * buffer_h;
    std::vector<int> x_offsets(dst_w);
    for (int x = 0; x < dst_w; x++) x_offsets[x] = crop_x + (int)((int64_t)x * w / dst_w);
    for (int y = 0; y < dst_h; y++) {
        int sy = crop_y + (int)((int64_t)y * h / dst_h);
        const uint8_t* yrow = Y + (size_t)sy * stride;
        const uint8_t* uvrow = UV + (size_t)(sy / 2) * stride;
        uint8_t* drow = rgba + (size_t)y * dst_w * 4;
        for (int x = 0; x < dst_w; x++) {
            int sx = x_offsets[x];
            int yy = yrow[sx];
            int u = uvrow[(sx / 2) * 2];
            int v = uvrow[(sx / 2) * 2 + 1];
            int c = yy - 16, d = u - 128, e = v - 128;
            drow[x * 4 + 0] = clamp8((298 * c + 409 * e + 128) >> 8);
            drow[x * 4 + 1] = clamp8((298 * c - 100 * d - 208 * e + 128) >> 8);
            drow[x * 4 + 2] = clamp8((298 * c + 516 * d + 128) >> 8);
            drow[x * 4 + 3] = 0xFF;
        }
    }
}

// Scans an Annex-B H.264 bitstream for the presence of an IDR NAL (type 5).
static bool contains_idr(const uint8_t* data, size_t len) {
    for (size_t i = 0; i + 4 < len; i++) {
        if (data[i] == 0 && data[i + 1] == 0) {
            size_t off;
            if (data[i + 2] == 1) off = i + 3;
            else if (data[i + 2] == 0 && data[i + 3] == 1) off = i + 4;
            else continue;
            if (off < len && (data[off] & 0x1F) == 5) return true;
        }
    }
    return false;
}

// ---------------------------------------------------------------------------
// Encoder
// ---------------------------------------------------------------------------
struct h264_encoder {
    FrameWait frame_wait;
    bool profile = false;
    int profile_frames = 0;
    int64_t profile_convert_ns = 0, profile_copy_ns = 0, profile_transform_ns = 0;
    ComPtr<IMFTransform> xform;
    ComPtr<IMFMediaEventGenerator> events;
    bool asynchronous = false;
    bool hardware = false;
    int pending_input = 0, pending_output = 0;
    int in_flight = 0;
    std::chrono::steady_clock::time_point oldest_input;
    int64_t last_convert_ns = 0, last_copy_ns = 0;
    int in_w = 0, in_h = 0, out_w = 0, out_h = 0, fps = 15;
    int y_stride = 0;
    int64_t frame_idx = 0;
    std::vector<uint8_t> bitstream;
    bool ok = false;
    const char* info_ = "none";

    // Enumerate encoders matching in/out types under `flags` and activate the
    // first one that succeeds. Returns false if none enumerated or all fail to
    // activate (callers then try the next flag set).
    bool activate_first(const MFT_REGISTER_TYPE_INFO& in_t,
                        const MFT_REGISTER_TYPE_INFO& out_t,
                        DWORD flags, bool& is_hw) {
        IMFActivate** acts = nullptr;
        UINT32 count = 0;
        HRESULT hr = MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER, flags,
                               &in_t, &out_t, &acts, &count);
        if (FAILED(hr) || count == 0) return false;
        bool okact = false;
        for (UINT32 i = 0; i < count && !okact; i++) {
            HRESULT h2 = acts[i]->ActivateObject(kIID_IMFTransform, (void**)xform.put());
            if (SUCCEEDED(h2) && xform) okact = true;
        }
        for (UINT32 i = 0; i < count; i++) acts[i]->Release();
        CoTaskMemFree(acts);
        if (!okact) { xform.reset(); return false; }
        is_hw = (flags & MFT_ENUM_FLAG_HARDWARE) != 0;
        return true;
    }

    bool create(int iw, int ih, int ow, int oh, int f, int bitrate_kbps,
                bool allow_hw = true) {
        in_w = iw; in_h = ih; out_w = ow; out_h = oh;
        if (f > 0) fps = f;
        y_stride = ((ow + 15) / 16) * 16;
        ensure_mf();
        char profile_flag[2] = {};
        profile = GetEnvironmentVariableA("RELAYNET_MEDIA_PROFILE", profile_flag, sizeof(profile_flag)) && profile_flag[0] == '1';

        MFT_REGISTER_TYPE_INFO in_t = {MFMediaType_Video, MFVideoFormat_NV12};
        MFT_REGISTER_TYPE_INFO out_t = {MFMediaType_Video, MFVideoFormat_H264};

        bool is_hw = false;
        if (!(allow_hw && activate_first(in_t, out_t,
                    MFT_ENUM_FLAG_HARDWARE | MFT_ENUM_FLAG_SORTANDFILTER, is_hw))) {
            if (!activate_first(in_t, out_t, MFT_ENUM_FLAG_SYNCMFT, is_hw)) {
                mf_log("H264 encoder not found");
                return false;
            }
        }
        info_ = is_hw ? "hardware" : "software";
        hardware = is_hw;
        auto retry_software = [&]() {
            if (!is_hw) return false;
            destroy();
            return create(iw, ih, ow, oh, f, bitrate_kbps, false);
        };
        HRESULT hr = S_OK;

        ComPtr<IMFAttributes> encoder_attrs;
        asynchronous = false;
        pending_input = pending_output = in_flight = 0;
        if (SUCCEEDED(xform->GetAttributes(encoder_attrs.put()))) {
            UINT32 async_flag = 0;
            encoder_attrs->GetUINT32(gMF_TRANSFORM_ASYNC, &async_flag);
            asynchronous = async_flag != 0;
            if (asynchronous &&
                FAILED(encoder_attrs->SetUINT32(gMF_TRANSFORM_ASYNC_UNLOCK, 1))) return retry_software();
            encoder_attrs->SetUINT32(gMF_LOW_LATENCY, 1);
        }
        if (asynchronous && FAILED(xform->QueryInterface(kIID_IMFMediaEventGenerator, (void**)events.put()))) {
            return retry_software();
        }
        // Low-latency / bitrate / GOP settings via ICodecAPI. Target bitrate is
        // set here (not just on the output media type) because some MFTs reject
        // modified output types and silently ignore MF_MT_AVG_BITRATE — the
        // ICodecAPI path is the one that reliably caps bandwidth.
        ComPtr<ICodecAPI> codec;
        if (SUCCEEDED(xform->QueryInterface(kIID_ICodecAPI, (void**)codec.put()))) {
            VARIANT v;
            VariantInit(&v); v.vt = VT_BOOL; v.boolVal = VARIANT_TRUE;
            HRESULT low_hr = codec->SetValue(&CODECAPI_AVLowLatencyMode, &v);
            if (FAILED(low_hr)) mf_log("encoder low latency unavailable (0x%08lx)", (unsigned long)low_hr);
            (void)codec->SetValue(&CODECAPI_AVEncCommonRealTime, &v);
            VariantInit(&v); v.vt = VT_UI4; v.ulVal = 0;
            (void)codec->SetValue(&CODECAPI_AVEncMPVDefaultBPictureCount, &v);
            if (bitrate_kbps > 0) {
                VariantInit(&v); v.vt = VT_UI4;
                v.ulVal = (ULONG)((unsigned long long)bitrate_kbps * 1000);
                (void)codec->SetValue(&CODECAPI_AVEncCommonMeanBitRate, &v);
            }
            // Intra refresh roughly once per second for loss recovery.
            if (fps > 0) {
                VariantInit(&v); v.vt = VT_UI4; v.ulVal = (ULONG)fps;
                (void)codec->SetValue(&CODECAPI_AVEncMPVGOPSize, &v);
            }
        }

        // Input type: NV12, progressive, target size + per-row stride.
        // The Microsoft H.264 encoder requires its output type first.
        ComPtr<IMFMediaType> imt;
        if (FAILED(MFCreateMediaType(imt.put()))) return retry_software();
        imt->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
        imt->SetGUID(MF_MT_SUBTYPE, MFVideoFormat_NV12);
        imt->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
        MFSetAttributeSize(imt.get(), MF_MT_FRAME_SIZE, (UINT32)out_w, (UINT32)out_h);
        imt->SetUINT32(gMF_MT_DEFAULT_STRIDE, (UINT32)y_stride);
        if (fps > 0) MFSetAttributeRatio(imt.get(), MF_MT_FRAME_RATE, (UINT32)fps, 1);

        // An encoder may require its output type before it can enumerate
        // available types, so create the H.264 type explicitly.
        ComPtr<IMFMediaType> omt;
        hr = MFCreateMediaType(omt.put());
        if (FAILED(hr)) return retry_software();
        (void)omt->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
        (void)omt->SetGUID(MF_MT_SUBTYPE, MFVideoFormat_H264);
        (void)omt->SetUINT32(MF_MT_AVG_BITRATE, (UINT32)bitrate_kbps * 1000);
        (void)MFSetAttributeSize(omt.get(), MF_MT_FRAME_SIZE, (UINT32)out_w, (UINT32)out_h);
        if (fps > 0) (void)MFSetAttributeRatio(omt.get(), MF_MT_FRAME_RATE, (UINT32)fps, 1);
        (void)omt->SetUINT32(gMF_LOW_LATENCY, 1);
        (void)omt->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
        hr = xform->SetOutputType(0, omt.get(), 0);
        if (FAILED(hr)) {
            mf_log("%s H264 encoder rejected output type (0x%08lx)", info_, (unsigned long)hr);
            return retry_software();
        }

        hr = xform->SetInputType(0, imt.get(), 0);
        if (FAILED(hr)) {
            mf_log("%s H264 encoder rejected input type (0x%08lx)", info_, (unsigned long)hr);
            return retry_software();
        }

        xform->ProcessMessage(MFT_MESSAGE_COMMAND_FLUSH, 0);
        xform->ProcessMessage(MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
        xform->ProcessMessage(MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0);
        ok = true;
        mf_log("H264 encoder ready %dx%d @%dfps %dkbps (%s)",
               out_w, out_h, fps, bitrate_kbps, info_);
        return true;
    }

    bool collect_outputs(std::vector<uint8_t>& out, int64_t& output_pts, int limit = 16) {
        for (int guard = 0; guard < limit; guard++) {
            if (asynchronous) {
                if (!poll_events()) return false;
                if (pending_output == 0) break;
                pending_output--;
            }
            MFT_OUTPUT_DATA_BUFFER odb;
            memset(&odb, 0, sizeof(odb));
            MFT_OUTPUT_STREAM_INFO info = {};
            if (FAILED(xform->GetOutputStreamInfo(0, &info))) return false;
            ComPtr<IMFSample> provided;
            if (!(info.dwFlags & (MFT_OUTPUT_STREAM_PROVIDES_SAMPLES |
                                  MFT_OUTPUT_STREAM_CAN_PROVIDE_SAMPLES))) {
                ComPtr<IMFMediaBuffer> buffer;
                DWORD size = info.cbSize ? info.cbSize : (DWORD)(out_w * out_h);
                if (FAILED(MFCreateSample(provided.put())) ||
                    FAILED(MFCreateMemoryBuffer(size, buffer.put())) ||
                    FAILED(provided->AddBuffer(buffer.get()))) return false;
                odb.pSample = provided.get();
            }
            DWORD status = 0;
            HRESULT hr = xform->ProcessOutput(0, 1, &odb, &status);
            ComPtr<IMFSample> returned(odb.pSample != provided.get() ? odb.pSample : nullptr);
            if (odb.pEvents) odb.pEvents->Release();
            if (hr == MF_E_TRANSFORM_NEED_MORE_INPUT) break;
            if (FAILED(hr)) {
                if (frame_idx <= 5) mf_log("H264 encoder ProcessOutput failed (0x%08lx)", (unsigned long)hr);
                return false;
            }
            IMFSample* sample = odb.pSample;
            if (!sample) return false;
            LONGLONG sample_pts = 0;
            if (SUCCEEDED(sample->GetSampleTime(&sample_pts))) output_pts = sample_pts * 100;
            DWORD nbuf = 0;
            if (FAILED(sample->GetBufferCount(&nbuf))) return false;
            for (DWORD i = 0; i < nbuf; i++) {
                ComPtr<IMFMediaBuffer> mb;
                if (FAILED(sample->GetBufferByIndex(i, mb.put()))) continue;
                BYTE* p = nullptr;
                DWORD len = 0;
                if (SUCCEEDED(mb->Lock(&p, nullptr, &len))) {
                    out.insert(out.end(), p, p + len);
                    mb->Unlock();
                }
            }
        }
        return true;
    }

    bool poll_events() {
        if (!asynchronous) return true;
        for (int guard = 0; guard < 64; guard++) {
            ComPtr<IMFMediaEvent> event;
            HRESULT hr = events->GetEvent(MF_EVENT_FLAG_NO_WAIT, event.put());
            if (hr == MF_E_NO_EVENTS_AVAILABLE) break;
            if (FAILED(hr)) return false;
            HRESULT status = S_OK;
            event->GetStatus(&status);
            if (FAILED(status)) {
                mf_log("GPU encoder event failed (0x%08lx)", (unsigned long)status);
                return false;
            }
            MediaEventType type;
            if (FAILED(event->GetType(&type))) return false;
            if (type == METransformNeedInput) pending_input++;
            if (type == METransformHaveOutput) pending_output++;
        }
        return true;
    }

    bool prepare_input(const uint8_t* pixels, int stride, bool swap_rb,
                       int64_t timestamp_ns, ComPtr<IMFSample>& sample) {
        auto start = std::chrono::steady_clock::now();
        ComPtr<IMFMediaBuffer> buf;
        DWORD bytes = (DWORD)((size_t)y_stride * out_h * 3 / 2);
        if (FAILED(MFCreateMemoryBuffer(bytes, buf.put()))) return false;
        BYTE* dst = nullptr;
        if (FAILED(buf->Lock(&dst, nullptr, nullptr))) return false;
        auto allocated = std::chrono::steady_clock::now();
        // Write straight into the sample owned by MF instead of copying a
        // second full NV12 image. The encoder retains this buffer as needed.
        scale_to_nv12(pixels, stride, in_w, in_h, out_w, out_h, y_stride, swap_rb, dst);
        auto converted = std::chrono::steady_clock::now();
        buf->Unlock();
        buf->SetCurrentLength(bytes);
        if (FAILED(MFCreateSample(sample.put()))) return false;
        sample->AddBuffer(buf.get());
        sample->SetSampleTime(timestamp_ns / 100);
        sample->SetSampleDuration(10000000LL / fps);
        frame_idx++;
        last_convert_ns = std::chrono::duration_cast<std::chrono::nanoseconds>(converted - allocated).count();
        last_copy_ns = std::chrono::duration_cast<std::chrono::nanoseconds>(allocated - start).count() +
                       std::chrono::duration_cast<std::chrono::nanoseconds>(std::chrono::steady_clock::now() - converted).count();
        return true;
    }

    void record_profile(int64_t transform_ns, const char* mode) {
        if (!profile) return;
        profile_convert_ns += last_convert_ns;
        profile_copy_ns += last_copy_ns;
        profile_transform_ns += transform_ns;
        if (++profile_frames == 120) {
            mf_log("encode %s profile %dx%d: convert=%.2fms buffer=%.2fms transform=%.2fms", mode, out_w, out_h,
                   profile_convert_ns / 120e6, profile_copy_ns / 120e6, profile_transform_ns / 120e6);
            profile_frames = 0;
            profile_convert_ns = profile_copy_ns = profile_transform_ns = 0;
        }
    }

    int submit(const uint8_t* pixels, int stride, bool swap_rb, int64_t timestamp_ns) {
        if (!ok || !asynchronous || !pixels || !poll_events()) return -1;
        // Two inputs overlap CPU preparation with GPU work without building
        // an unbounded latency queue. A busy raw picture was never encoded.
        if (pending_input == 0 || in_flight >= 2) return 1;
        ComPtr<IMFSample> sample;
        if (!prepare_input(pixels, stride, swap_rb, timestamp_ns, sample)) return -1;
        auto transform_start = std::chrono::steady_clock::now();
        if (FAILED(xform->ProcessInput(0, sample.get(), 0))) return -1;
        pending_input--;
        if (in_flight++ == 0) oldest_input = std::chrono::steady_clock::now();
        record_profile(std::chrono::duration_cast<std::chrono::nanoseconds>(std::chrono::steady_clock::now() - transform_start).count(), "pipeline");
        return 0;
    }

    int poll(unsigned char** out_data, int* out_len, int* out_keyframe, int64_t* output_timestamp_ns) {
        *out_data = nullptr; *out_len = 0; *out_keyframe = 0; *output_timestamp_ns = 0;
        if (!ok || !asynchronous || !poll_events()) return -1;
        if (pending_output == 0) {
            if (in_flight > 0 && std::chrono::steady_clock::now() - oldest_input > std::chrono::milliseconds(500)) return -1;
            return 0;
        }
        bitstream.clear();
        int64_t pts = 0;
        if (!collect_outputs(bitstream, pts, 1) || bitstream.empty()) return -1;
        if (in_flight > 0) in_flight--;
        oldest_input = std::chrono::steady_clock::now();
        auto out = (unsigned char*)malloc(bitstream.size());
        if (!out) return -1;
        memcpy(out, bitstream.data(), bitstream.size());
        *out_data = out; *out_len = (int)bitstream.size();
        *out_keyframe = contains_idr(out, bitstream.size()) ? 1 : 0;
        *output_timestamp_ns = pts;
        return 0;
    }

    int send(const uint8_t* pixels, int stride, bool swap_rb, int64_t timestamp_ns,
             unsigned char** out_data, int* out_len, int* out_keyframe, int64_t* output_timestamp_ns) {
        *out_data = nullptr; *out_len = 0; *out_keyframe = 0;
        *output_timestamp_ns = timestamp_ns;
        if (!ok || pixels == nullptr) return -1;

        if (asynchronous) {
            auto deadline = std::chrono::steady_clock::now() + std::chrono::milliseconds(100);
            while (pending_input == 0) {
                if (!poll_events()) return -1;
                if (pending_input != 0) break;
                if (std::chrono::steady_clock::now() >= deadline) return -1;
                frame_wait.wait();
            }
        }

        ComPtr<IMFSample> sample;
        if (!prepare_input(pixels, stride, swap_rb, timestamp_ns, sample)) return -1;
        auto profile_copied = std::chrono::steady_clock::now();

        // 3) Feed. If the MFT is not accepting (outputs pending), drain once
        //    and retry — the drained data belongs to the previous frame but is
        //    still a valid access unit, so the caller ships it as-is.
        bitstream.clear();
        int64_t output_pts = timestamp_ns;
        HRESULT hr = xform->ProcessInput(0, sample.get(), 0);
        if (hr == (HRESULT)MF_E_NOTACCEPTING) {
            if (asynchronous) return -1;
            if (!collect_outputs(bitstream, output_pts)) return -1;
            hr = xform->ProcessInput(0, sample.get(), 0);
        }
        if (asynchronous) {
            if (FAILED(hr)) return -1;
            pending_input--;
        }
        if (FAILED(hr) && bitstream.empty()) return 0;

        if (!collect_outputs(bitstream, output_pts)) return -1;
        if (asynchronous && bitstream.empty()) {
            auto deadline = std::chrono::steady_clock::now() + std::chrono::milliseconds(100);
            while (bitstream.empty()) {
                if (!poll_events()) return -1;
                if (!collect_outputs(bitstream, output_pts)) return -1;
                if (!bitstream.empty()) break;
                if (std::chrono::steady_clock::now() >= deadline) return -1;
                frame_wait.wait();
            }
        }
        if (bitstream.empty()) return 0;

        // 4) Copy out of the reusable buffer into a malloc'd block (Go side
        //    frees via h264_buf_free).
        size_t n = bitstream.size();
        unsigned char* out = (unsigned char*)malloc(n);
        if (!out) return -1;
        memcpy(out, bitstream.data(), n);
        *out_data = out;
        *out_len = (int)n;
        *out_keyframe = contains_idr(out, n) ? 1 : 0;
        *output_timestamp_ns = output_pts;
        record_profile(std::chrono::duration_cast<std::chrono::nanoseconds>(std::chrono::steady_clock::now() - profile_copied).count(), "sync");
        return 0;
    }

    void destroy() {
        ok = false;
        if (xform) {
            xform->ProcessMessage(MFT_MESSAGE_NOTIFY_END_OF_STREAM, 0);
            xform->ProcessMessage(MFT_MESSAGE_COMMAND_FLUSH, 0);
            if (asynchronous) {
                ComPtr<IMFShutdown> shutdown;
                if (SUCCEEDED(xform->QueryInterface(kIID_IMFShutdown, (void**)shutdown.put()))) shutdown->Shutdown();
            }
            events.reset();
            xform.reset();
        }
    }
};

// ---------------------------------------------------------------------------
// Decoder
// ---------------------------------------------------------------------------
struct h264_decoder {
    ComPtr<IMFTransform> xform;
    int out_w = 0, out_h = 0, out_stride = 0;
    int buffer_h = 0, crop_x = 0, crop_y = 0;
    int64_t frame_idx = 0;
    bool ok = false;

    bool create() {
        ensure_mf();

        MFT_REGISTER_TYPE_INFO in_t = {MFMediaType_Video, MFVideoFormat_H264};
        IMFActivate** acts = nullptr;
        UINT32 count = 0;
        HRESULT hr = MFTEnumEx(MFT_CATEGORY_VIDEO_DECODER, MFT_ENUM_FLAG_SYNCMFT,
                               &in_t, nullptr, &acts, &count);
        if (FAILED(hr) || count == 0) {
            mf_log("H264 decoder not found");
            return false;
        }
        hr = acts[0]->ActivateObject(kIID_IMFTransform, (void**)xform.put());
        for (UINT32 i = 0; i < count; i++) acts[i]->Release();
        CoTaskMemFree(acts);
        if (FAILED(hr) || !xform) return false;

        ComPtr<IMFAttributes> attrs;
        if (SUCCEEDED(xform->GetAttributes(attrs.put()))) {
            (void)attrs->SetUINT32(gMF_LOW_LATENCY, 1);
        }
        ComPtr<ICodecAPI> codec;
        if (SUCCEEDED(xform->QueryInterface(kIID_ICodecAPI, (void**)codec.put()))) {
            VARIANT low_latency;
            VariantInit(&low_latency);
            // The inbox H.264 decoder requires VT_UI4 for this property,
            // unlike the encoder's VT_BOOL (Microsoft codec API contract).
            low_latency.vt = VT_UI4;
            low_latency.ulVal = 1;
            HRESULT low_hr = codec->SetValue(&CODECAPI_AVLowLatencyMode, &low_latency);
            if (FAILED(low_hr)) mf_log("decoder low latency unavailable (0x%08lx)", (unsigned long)low_hr);
        }

        // Input type: H264 Annex-B byte stream (no sequence-header blob).
        ComPtr<IMFMediaType> imt;
        if (FAILED(MFCreateMediaType(imt.put()))) return false;
        imt->SetGUID(MF_MT_MAJOR_TYPE, MFMediaType_Video);
        imt->SetGUID(MF_MT_SUBTYPE, MFVideoFormat_H264);
        imt->SetUINT32(MF_MT_INTERLACE_MODE, MFVideoInterlace_Progressive);
        hr = xform->SetInputType(0, imt.get(), 0);
        if (FAILED(hr)) {
            mf_log("H264 decoder SetInputType failed (0x%08lx)", (unsigned long)hr);
            return false;
        }

        // Output type: prefer NV12 (universally supported by H264 decoders).
        bool set = false;
        for (DWORD i = 0; ; i++) {
            ComPtr<IMFMediaType> omt;
            if (FAILED(xform->GetOutputAvailableType(0, i, omt.put()))) break;
            GUID sub;
            if (SUCCEEDED(omt->GetGUID(MF_MT_SUBTYPE, &sub)) &&
                IsEqualGUID(sub, MFVideoFormat_NV12)) {
                hr = xform->SetOutputType(0, omt.get(), 0);
                set = SUCCEEDED(hr);
                break;
            }
        }
        if (!set) {
            mf_log("H264 decoder has no NV12 output");
            return false;
        }

        xform->ProcessMessage(MFT_MESSAGE_COMMAND_FLUSH, 0);
        xform->ProcessMessage(MFT_MESSAGE_NOTIFY_BEGIN_STREAMING, 0);
        xform->ProcessMessage(MFT_MESSAGE_NOTIFY_START_OF_STREAM, 0);
        ok = true;
        return true;
    }

    bool query_output_format() {
        if (out_w != 0) return true;
        ComPtr<IMFMediaType> ct;
        if (FAILED(xform->GetOutputCurrentType(0, ct.put()))) return false;
        UINT32 w = 0, h = 0;
        if (FAILED(MFGetAttributeSize(ct.get(), MF_MT_FRAME_SIZE, &w, &h))) return false;
        if (w == 0 || h == 0) return false;
        UINT32 st = 0;
        if (FAILED(ct->GetUINT32(gMF_MT_DEFAULT_STRIDE, &st)) || st == 0) st = w;
        out_w = (int)w; out_h = (int)h; out_stride = (int)st;
        buffer_h = (int)h;
        crop_x = crop_y = 0;
        // H.264 stores macroblock padding (1080p may be 1088 rows).
        // Keep the buffer height for UV offsets, but display only its aperture.
        MFVideoArea area = {};
        UINT32 blob_size = 0;
        if (SUCCEEDED(ct->GetBlob(MF_MT_MINIMUM_DISPLAY_APERTURE,
                (UINT8*)&area, sizeof(area), &blob_size)) && blob_size == sizeof(area) &&
                area.OffsetX.value >= 0 && area.OffsetY.value >= 0 &&
                area.Area.cx > 0 && area.Area.cy > 0 &&
                area.OffsetX.value + area.Area.cx <= (int)w &&
                area.OffsetY.value + area.Area.cy <= (int)h) {
            crop_x = area.OffsetX.value; crop_y = area.OffsetY.value;
            out_w = area.Area.cx; out_h = area.Area.cy;
        }
        return true;
    }

    int feed(const uint8_t* data, int len, int64_t timestamp_ns,
             unsigned char** out_rgba, int* out_w_, int* out_h_, int64_t* output_timestamp_ns,
             bool convert = true, int max_w = 0, int max_h = 0) {
        *out_rgba = nullptr; *out_w_ = 0; *out_h_ = 0;
        *output_timestamp_ns = timestamp_ns;
        if (!ok || data == nullptr || len <= 0) return -1;

        ComPtr<IMFMediaBuffer> buf;
        if (FAILED(MFCreateMemoryBuffer((DWORD)len, buf.put()))) return -1;
        {
            BYTE* dst = nullptr;
            if (FAILED(buf->Lock(&dst, nullptr, nullptr))) return -1;
            memcpy(dst, data, len);
            buf->Unlock();
            buf->SetCurrentLength((DWORD)len);
        }
        ComPtr<IMFSample> sample;
        if (FAILED(MFCreateSample(sample.put()))) return -1;
        sample->AddBuffer(buf.get());
        LONGLONG t = timestamp_ns / 100;
        frame_idx++;
        sample->SetSampleTime(t);
        sample->SetSampleDuration(333333);

        HRESULT hr = xform->ProcessInput(0, sample.get(), 0);
        if (hr == (HRESULT)MF_E_NOTACCEPTING) {
            // Do not silently lose an encoded reference frame.
            return -1;
        }
        if (FAILED(hr)) {
            // Broken stream state (e.g. lost fragment under a P-frame chain):
            // surface as error so Go recreates the decoder and waits for IDR.
            return -1;
        }

        // Drain; remember the LAST converted frame (initial IDR may yield the
        // SPS setup frame with no pixels, so loop until no more output).
        unsigned char* last = nullptr;
        int last_w = 0, last_h = 0;
        for (int guard = 0; guard < 16; guard++) {
            MFT_OUTPUT_DATA_BUFFER odb;
            memset(&odb, 0, sizeof(odb));
            MFT_OUTPUT_STREAM_INFO info = {};
            if (FAILED(xform->GetOutputStreamInfo(0, &info))) break;
            ComPtr<IMFSample> provided;
            if (!(info.dwFlags & (MFT_OUTPUT_STREAM_PROVIDES_SAMPLES |
                                  MFT_OUTPUT_STREAM_CAN_PROVIDE_SAMPLES))) {
                ComPtr<IMFMediaBuffer> buffer;
                DWORD size = info.cbSize ? info.cbSize : 1024 * 1024;
                if (FAILED(MFCreateSample(provided.put())) ||
                    FAILED(MFCreateMemoryBuffer(size, buffer.put())) ||
                    FAILED(provided->AddBuffer(buffer.get()))) break;
                odb.pSample = provided.get();
            }
            DWORD status = 0;
            HRESULT h2 = xform->ProcessOutput(0, 1, &odb, &status);
            ComPtr<IMFSample> returned(odb.pSample != provided.get() ? odb.pSample : nullptr);
            if (odb.pEvents) odb.pEvents->Release();
            if (h2 == MF_E_TRANSFORM_STREAM_CHANGE) {
                bool set = false;
                for (DWORD i = 0; ; i++) {
                    ComPtr<IMFMediaType> mt;
                    if (FAILED(xform->GetOutputAvailableType(0, i, mt.put()))) break;
                    GUID sub;
                    if (SUCCEEDED(mt->GetGUID(MF_MT_SUBTYPE, &sub)) &&
                        IsEqualGUID(sub, MFVideoFormat_NV12) &&
                        SUCCEEDED(xform->SetOutputType(0, mt.get(), 0))) {
                        out_w = out_h = out_stride = 0;
                        set = true;
                        break;
                    }
                }
                if (set) continue;
                break;
            }
            if (h2 == MF_E_TRANSFORM_NEED_MORE_INPUT) break;
            if (FAILED(h2)) {
                mf_log("H264 decoder ProcessOutput failed (0x%08lx)", (unsigned long)h2);
                break;
            }
            IMFSample* s = odb.pSample;
            if (!s) break;

            if (!query_output_format()) continue;
            // Expired pictures still pass through the decoder so future
            // P-frames retain their reference chain. Avoid RGBA conversion
            // and three large image copies for pictures that cannot be shown.
            if (!convert) continue;
            int display_w = out_w, display_h = out_h;
            if (max_w > 0 && max_h > 0 && (display_w > max_w || display_h > max_h)) {
                if ((int64_t)max_w * out_h <= (int64_t)max_h * out_w) {
                    display_w = max_w;
                    display_h = (int)((int64_t)out_h * max_w / out_w);
                } else {
                    display_h = max_h;
                    display_w = (int)((int64_t)out_w * max_h / out_h);
                }
                if (display_w < 1) display_w = 1;
                if (display_h < 1) display_h = 1;
            }

            DWORD nbuf = 0;
            if (FAILED(s->GetBufferCount(&nbuf))) continue;
            for (DWORD i = 0; i < nbuf; i++) {
                ComPtr<IMFMediaBuffer> mb;
                if (FAILED(s->GetBufferByIndex(i, mb.put()))) continue;
                BYTE* p = nullptr;
                DWORD blen = 0;
                if (FAILED(mb->Lock(&p, nullptr, &blen))) continue;
                if (blen >= (DWORD)(out_stride * buffer_h * 3 / 2)) {
                    if (last) free(last);
                    last = (unsigned char*)malloc((size_t)display_w * display_h * 4);
                    if (last) {
                        nv12_to_rgba(p, out_stride, buffer_h, crop_x, crop_y,
                                     out_w, out_h, display_w, display_h, last);
                        last_w = display_w; last_h = display_h;
                        LONGLONG output_pts = 0;
                        if (SUCCEEDED(s->GetSampleTime(&output_pts))) *output_timestamp_ns = output_pts * 100;
                    }
                }
                mb->Unlock();
            }
        }
        *out_rgba = last;
        *out_w_ = last_w;
        *out_h_ = last_h;
        return 0;
    }

    void destroy() {
        ok = false;
        if (xform) {
            xform->ProcessMessage(MFT_MESSAGE_NOTIFY_END_OF_STREAM, 0);
            xform->ProcessMessage(MFT_MESSAGE_COMMAND_FLUSH, 0);
            xform.reset();
        }
    }
};

// ---------------------------------------------------------------------------
// C API
// ---------------------------------------------------------------------------
h264_encoder* h264_enc_create(int in_w, int in_h, int out_w, int out_h,
                              int fps, int bitrate_kbps) {
    h264_encoder* e = new h264_encoder();
    if (!e->create(in_w, in_h, out_w, out_h, fps, bitrate_kbps)) {
        delete e;
        return nullptr;
    }
    return e;
}

int h264_enc_send(h264_encoder* e, const unsigned char* pixels, int stride,
                  int swap_rb, int64_t timestamp_ns, unsigned char** out_data, int* out_len,
                  int* out_keyframe, int64_t* output_timestamp_ns) {
    if (!e) return -1;
    return e->send(pixels, stride, swap_rb != 0, timestamp_ns, out_data, out_len, out_keyframe, output_timestamp_ns);
}

void h264_enc_destroy(h264_encoder* e) {
    if (!e) return;
    e->destroy();
    delete e;
}

int h264_enc_is_hardware(h264_encoder* e) {
    return e && e->hardware ? 1 : 0;
}

int h264_enc_submit(h264_encoder* e, const unsigned char* pixels, int stride,
                    int swap_rb, int64_t timestamp_ns) {
    return e ? e->submit(pixels, stride, swap_rb != 0, timestamp_ns) : -1;
}

int h264_enc_poll(h264_encoder* e, unsigned char** out_data, int* out_len,
                  int* out_keyframe, int64_t* output_timestamp_ns) {
    return e ? e->poll(out_data, out_len, out_keyframe, output_timestamp_ns) : -1;
}

h264_decoder* h264_dec_create(void) {
    h264_decoder* d = new h264_decoder();
    if (!d->create()) {
        delete d;
        return nullptr;
    }
    return d;
}

int h264_dec_feed(h264_decoder* d, const unsigned char* data, int len, int64_t timestamp_ns,
                  int max_w, int max_h,
                  unsigned char** out_rgba, int* out_w, int* out_h, int64_t* output_timestamp_ns) {
    if (!d) return -1;
    return d->feed(data, len, timestamp_ns, out_rgba, out_w, out_h, output_timestamp_ns, true, max_w, max_h);
}

int h264_dec_skip(h264_decoder* d, const unsigned char* data, int len, int64_t timestamp_ns) {
    if (!d) return -1;
    unsigned char* pixels = nullptr;
    int w = 0, h = 0;
    int64_t output_pts = 0;
    return d->feed(data, len, timestamp_ns, &pixels, &w, &h, &output_pts, false);
}

void h264_dec_destroy(h264_decoder* d) {
    if (!d) return;
    d->destroy();
    delete d;
}

void h264_buf_free(void* p) {
    free(p);
}
