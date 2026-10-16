// WGC (Windows Graphics Capture) screen capture implementation.
// Uses WinRT APIs (Windows.Graphics.Capture) with Direct3D11.
//
// MinGW's WinRT headers (windows.graphics.capture.h -> windows.foundation.h)
// have a template redefinition bug (IReference<boolean> vs IReference<BYTE>,
// both mapping to unsigned char), so we define the minimal COM interfaces we
// need manually instead of including those headers.

#include "wgc_capture.hpp"

#include <windows.h>
#include <d3d11.h>
#include <dxgi.h>
#include <wrl/client.h>
#include <roapi.h>
#include <winstring.h>
#include <inspectable.h>
#include "frame_wait.hpp"

#include <atomic>
#include <cstdio>
#include <thread>

using Microsoft::WRL::ComPtr;

// ---------------------------------------------------------------------------
// Minimal WinRT type definitions
// ---------------------------------------------------------------------------

struct SizeInt32 {
    INT32 Width;
    INT32 Height;
};

struct EventRegistrationToken {
    INT64 Value;
};

enum DirectXPixelFormat {
    DirectXPixelFormat_B8G8R8A8UIntNormalized = 87,
};

// IClosable (windows.foundation.IClosable)
struct IClosable : IInspectable {
    virtual HRESULT STDMETHODCALLTYPE Close() = 0;
};

// Forward declarations
struct IDirect3DDevice;
struct IDirect3DSurface;
struct IGraphicsCaptureItem;
struct IGraphicsCaptureSession;
struct IDirect3D11CaptureFrame;
struct IDirect3D11CaptureFramePool;

// IDirect3DDevice (windows.graphics.directx.direct3d11.IDirect3DDevice)
struct IDirect3DDevice : IInspectable {
    // We don't call any methods directly; only used as a handle type.
};

// IDirect3DSurface (windows.graphics.directx.direct3d11.IDirect3DSurface)
struct IDirect3DSurface : IInspectable {
    // We use IDirect3DDxgiInterfaceAccess to get the DXGI interface.
};

// IDirect3DDxgiInterfaceAccess — bridges WinRT Direct3D objects to DXGI.
// IID: {A9B3D012-3DF2-4EE3-B8D1-8695F457D3C1}
struct IDirect3DDxgiInterfaceAccess : IUnknown {
    virtual HRESULT STDMETHODCALLTYPE GetInterface(REFIID iid, void** p) = 0;
};

// IGraphicsCaptureItem
struct IGraphicsCaptureItem : IInspectable {
    virtual HRESULT STDMETHODCALLTYPE get_DisplayName(HSTRING* value) = 0;
    virtual HRESULT STDMETHODCALLTYPE get_Size(SizeInt32* value) = 0;
    virtual HRESULT STDMETHODCALLTYPE add_Closed(IUnknown* handler, EventRegistrationToken* token) = 0;
    virtual HRESULT STDMETHODCALLTYPE remove_Closed(EventRegistrationToken token) = 0;
};

// IGraphicsCaptureItemInterop (IUnknown-based)
struct IGraphicsCaptureItemInterop : IUnknown {
    virtual HRESULT STDMETHODCALLTYPE CreateForWindow(HWND window, REFIID iid, void** result) = 0;
    virtual HRESULT STDMETHODCALLTYPE CreateForMonitor(HMONITOR monitor, REFIID iid, void** result) = 0;
};

// IDirect3D11CaptureFrame
struct IDirect3D11CaptureFrame : IInspectable {
    virtual HRESULT STDMETHODCALLTYPE get_Surface(IDirect3DSurface** value) = 0;
    virtual HRESULT STDMETHODCALLTYPE get_SystemRelativeTime(UINT64* value) = 0;
    virtual HRESULT STDMETHODCALLTYPE get_ContentSize(SizeInt32* value) = 0;
    virtual HRESULT STDMETHODCALLTYPE get_DirtyRegions(void** value) = 0;
    virtual HRESULT STDMETHODCALLTYPE get_DirtyRegionMode(int* value) = 0;
};

// IDirect3D11CaptureFramePool
struct IDirect3D11CaptureFramePool : IInspectable {
    virtual HRESULT STDMETHODCALLTYPE Recreate(IDirect3DDevice* device, int pixelFormat, int numberOfBuffers, SizeInt32 size) = 0;
    virtual HRESULT STDMETHODCALLTYPE TryGetNextFrame(IDirect3D11CaptureFrame** frame) = 0;
    virtual HRESULT STDMETHODCALLTYPE add_FrameArrived(IUnknown* handler, EventRegistrationToken* token) = 0;
    virtual HRESULT STDMETHODCALLTYPE remove_FrameArrived(EventRegistrationToken token) = 0;
    virtual HRESULT STDMETHODCALLTYPE CreateCaptureSession(IGraphicsCaptureItem* item, IGraphicsCaptureSession** session) = 0;
    virtual HRESULT STDMETHODCALLTYPE get_DispatcherQueue(void** value) = 0;
};

// CreateFreeThreaded is on Statics2, not Statics.
struct IDirect3D11CaptureFramePoolStatics2 : IInspectable {
    virtual HRESULT STDMETHODCALLTYPE CreateFreeThreaded(IDirect3DDevice* device, int pixelFormat, int numberOfBuffers, SizeInt32 size, IDirect3D11CaptureFramePool** framePool) = 0;
};

// IGraphicsCaptureSession
struct IGraphicsCaptureSession : IInspectable {
    virtual HRESULT STDMETHODCALLTYPE StartCapture() = 0;
    virtual HRESULT STDMETHODCALLTYPE get_IsCursorCaptureEnabled(BOOL* value) = 0;
    virtual HRESULT STDMETHODCALLTYPE put_IsCursorCaptureEnabled(BOOL value) = 0;
    virtual HRESULT STDMETHODCALLTYPE get_IsBorderRequired(BOOL* value) = 0;
    virtual HRESULT STDMETHODCALLTYPE put_IsBorderRequired(BOOL value) = 0;
};

// Windows 11 24H2+ exposes capture pacing through a separate interface.
struct CaptureTimeSpan { INT64 Duration; };
struct IGraphicsCaptureSession5 : IInspectable {
    virtual HRESULT STDMETHODCALLTYPE get_MinUpdateInterval(CaptureTimeSpan* value) = 0;
    virtual HRESULT STDMETHODCALLTYPE put_MinUpdateInterval(CaptureTimeSpan value) = 0;
};

// IGraphicsCaptureSessionStatics
struct IGraphicsCaptureSessionStatics : IInspectable {
    virtual HRESULT STDMETHODCALLTYPE IsSupported(BOOL* value) = 0;
};

// ---------------------------------------------------------------------------
// GUIDs (matched to MinGW's windows.graphics.capture.h definitions)
// ---------------------------------------------------------------------------

static const GUID IID_IGraphicsCaptureItem =
    {0x79C3F95B, 0x31F7, 0x4EC2, {0xA4, 0x64, 0x63, 0x2E, 0xF5, 0xD3, 0x07, 0x60}};
static const GUID IID_IGraphicsCaptureItemInterop =
    {0x3628E81B, 0x3CAC, 0x4C60, {0xB7, 0xF4, 0x23, 0xCE, 0x0E, 0x0C, 0x33, 0x56}};
static const GUID IID_IGraphicsCaptureSession =
    {0x814E42A9, 0xF70F, 0x4AD7, {0x93, 0x9B, 0xFD, 0xDC, 0xC6, 0xEB, 0x88, 0x0D}};
static const GUID IID_IDirect3D11CaptureFrame =
    {0xFA50C623, 0x38DA, 0x4B32, {0xAC, 0xF3, 0xFA, 0x97, 0x34, 0xAD, 0x80, 0x0E}};
static const GUID IID_IDirect3D11CaptureFramePool =
    {0x24EB6D22, 0x1975, 0x422E, {0x82, 0xE7, 0x78, 0x0D, 0xBD, 0x8D, 0xDF, 0x24}};
static const GUID CLSID_Direct3D11CaptureFramePool =
    {0x000005F8, 0xAE2F, 0x40C4, {0x4D, 0x45, 0xA1, 0x4B, 0x3F, 0x6D, 0x59, 0x59}};
static const GUID CLSID_GraphicsCaptureItem =
    {0xD618AB3B, 0x8906, 0x4549, {0xB0, 0x8C, 0x24, 0x2E, 0x88, 0x3F, 0x28, 0x13}};

// Associate UUIDs with our interface types so IID_PPV_ARGS / __uuidof work.
__CRT_UUID_DECL(IDirect3DDevice,              0xa37624ab, 0x8d5f, 0x4650, 0x9d,0x3e, 0x9e,0xae,0x3d,0x9b,0xc6,0x70)
__CRT_UUID_DECL(IDirect3DSurface,             0x0bf4a146, 0x13c1, 0x4694, 0xbe,0xe3, 0x7a,0xbf,0x15,0xea,0xf5,0x86)
__CRT_UUID_DECL(IClosable,                    0x30d5a829, 0x7fa4, 0x4026, 0x83,0xbb, 0xd7,0x5b,0xae,0x4e,0xa9,0x9e)
__CRT_UUID_DECL(IGraphicsCaptureItem,         0x79c3f95b, 0x31f7, 0x4ec2, 0xa4,0x64, 0x63,0x2e,0xf5,0xd3,0x07,0x60)
__CRT_UUID_DECL(IGraphicsCaptureItemInterop,  0x3628e81b, 0x3cac, 0x4c60, 0xb7,0xf4, 0x23,0xce,0x0e,0x0c,0x33,0x56)
__CRT_UUID_DECL(IGraphicsCaptureSession,      0x814e42a9, 0xf70f, 0x4ad7, 0x93,0x9b, 0xfd,0xdc,0xc6,0xeb,0x88,0x0d)
__CRT_UUID_DECL(IGraphicsCaptureSession5,     0x67c0ea62, 0x1f85, 0x5061, 0x92,0x5a, 0x23,0x9b,0xe0,0xac,0x09,0xcb)
__CRT_UUID_DECL(IDirect3D11CaptureFrame,      0xfa50c623, 0x38da, 0x4b32, 0xac,0xf3, 0xfa,0x97,0x34,0xad,0x80,0x0e)
__CRT_UUID_DECL(IDirect3D11CaptureFramePool,  0x24eb6d22, 0x1975, 0x422e, 0x82,0xe7, 0x78,0x0d,0xbd,0x8d,0xdf,0x24)
__CRT_UUID_DECL(IDirect3D11CaptureFramePoolStatics2, 0x589b103f, 0x6bbc, 0x5df5, 0xa9,0x91, 0x02,0xe2,0x8b,0x3b,0x66,0xd5)
__CRT_UUID_DECL(IDirect3DDxgiInterfaceAccess, 0xa9b3d012, 0x3df2, 0x4ee3, 0xb8,0xd1, 0x86,0x95,0xf4,0x57,0xd3,0xc1)

// CreateDirect3D11DeviceFromDXGIDevice lives in d3d11.dll.
EXTERN_C HRESULT WINAPI CreateDirect3D11DeviceFromDXGIDevice(
    IDXGIDevice* dxgiDevice,
    IInspectable** graphicsDevice);

// ---------------------------------------------------------------------------
// Implementation
// ---------------------------------------------------------------------------

struct WgcCapture {
    int display_index;
    wgc_frame_cb callback;
    void* userdata;

    ComPtr<ID3D11Device> d3d_device;
    ComPtr<ID3D11DeviceContext> d3d_context;
    ComPtr<IDirect3DDevice> winrt_device;
    ComPtr<IGraphicsCaptureItem> capture_item;
    ComPtr<IDirect3D11CaptureFramePool> frame_pool;
    ComPtr<IGraphicsCaptureSession> session;

    std::atomic<bool> running{false};
    std::thread capture_thread;

    // Cached staging texture for CPU readback. Recreated only when the capture
    // dimensions change — creating a D3D11 texture per frame was a major
    // throughput bottleneck (several ms each), capping capture at ~30 FPS.
    ComPtr<ID3D11Texture2D> staging_tex;
    int staging_w = 0;
    int staging_h = 0;
};

// Check if WGC is supported (Windows 10 1903 / build 18362+ for CreateFreeThreaded).
// VerifyVersionInfoW is manifest-sensitive and reports Windows 8 on unmanifested
// Windows 10/11 desktop apps, which incorrectly sent Auto to the slow DXGI path.
int wgc_is_supported(void) {
    using RtlGetVersionFn = LONG (WINAPI *)(OSVERSIONINFOW*);
    HMODULE ntdll = GetModuleHandleW(L"ntdll.dll");
    auto rtlGetVersion = reinterpret_cast<RtlGetVersionFn>(
        GetProcAddress(ntdll, "RtlGetVersion"));
    if (!rtlGetVersion) return 0;

    OSVERSIONINFOW osvi = {};
    osvi.dwOSVersionInfoSize = sizeof(osvi);
    if (rtlGetVersion(&osvi) != 0) return 0;

    return osvi.dwMajorVersion > 10 ||
           (osvi.dwMajorVersion == 10 && osvi.dwBuildNumber >= 18362);
}

static HMONITOR GetMonitorFromIndex(int index) {
    struct EnumContext {
        int target;
        int current;
        HMONITOR result;
    } ctx = {index, 0, NULL};

    EnumDisplayMonitors(NULL, NULL, [](HMONITOR hMonitor, HDC, LPRECT, LPARAM lParam) -> BOOL {
        auto* c = reinterpret_cast<EnumContext*>(lParam);
        if (c->current == c->target) {
            c->result = hMonitor;
            return FALSE;
        }
        c->current++;
        return TRUE;
    }, reinterpret_cast<LPARAM>(&ctx));

    return ctx.result;
}

static WgcCapture* wgc_create_failed(WgcCapture* cap, const char* step, HRESULT hr) {
    fprintf(stderr, "[wgc] %s failed (0x%08lx)\n", step, (unsigned long)hr);
    fflush(stderr);
    delete cap;
    return nullptr;
}

WgcCapture* wgc_create(int display_index, wgc_frame_cb callback, void* userdata) {
    if (!wgc_is_supported()) {
        return nullptr;
    }

    HRESULT hr = RoInitialize(RO_INIT_MULTITHREADED);
    if (FAILED(hr) && hr != RPC_E_CHANGED_MODE) {
        fprintf(stderr, "[wgc] RoInitialize failed (0x%08lx)\n", (unsigned long)hr);
        return nullptr;
    }

    auto* cap = new (std::nothrow) WgcCapture();
    if (!cap) return wgc_create_failed(nullptr, "allocate capture", E_OUTOFMEMORY);
    cap->display_index = display_index;
    cap->callback = callback;
    cap->userdata = userdata;

    // 1. Create D3D11 device
    D3D_FEATURE_LEVEL feature_levels[] = {D3D_FEATURE_LEVEL_11_0, D3D_FEATURE_LEVEL_10_1, D3D_FEATURE_LEVEL_10_0};
    D3D_FEATURE_LEVEL created_level;
    hr = D3D11CreateDevice(nullptr, D3D_DRIVER_TYPE_HARDWARE, nullptr,
                            D3D11_CREATE_DEVICE_BGRA_SUPPORT | D3D11_CREATE_DEVICE_VIDEO_SUPPORT,
                            feature_levels, ARRAYSIZE(feature_levels),
                            D3D11_SDK_VERSION, &cap->d3d_device, &created_level, &cap->d3d_context);
    if (FAILED(hr)) {
        return wgc_create_failed(cap, "D3D11CreateDevice", hr);
    }

    // 2. Create WinRT IDirect3DDevice from D3D device
    ComPtr<IDXGIDevice> dxgi_device;
    hr = cap->d3d_device.As(&dxgi_device);
    if (FAILED(hr)) {
        return wgc_create_failed(cap, "Query IDXGIDevice", hr);
    }

    IInspectable* inspectable = nullptr;
    hr = CreateDirect3D11DeviceFromDXGIDevice(dxgi_device.Get(), &inspectable);
    if (FAILED(hr) || !inspectable) {
        return wgc_create_failed(cap, "CreateDirect3D11DeviceFromDXGIDevice", hr);
    }
    // IInspectable -> IDirect3DDevice
    hr = inspectable->QueryInterface(IID_PPV_ARGS(cap->winrt_device.GetAddressOf()));
    inspectable->Release();
    if (FAILED(hr)) {
        return wgc_create_failed(cap, "Query IDirect3DDevice", hr);
    }

    // 3. Get HMONITOR for display
    HMONITOR hmonitor = GetMonitorFromIndex(display_index);
    if (!hmonitor) {
        return wgc_create_failed(cap, "find monitor", E_HANDLE);
    }
    MONITORINFOEXW monitor_info = {};
    monitor_info.cbSize = sizeof(monitor_info);
    DEVMODEW display_mode = {};
    display_mode.dmSize = sizeof(display_mode);
    if (GetMonitorInfoW(hmonitor, &monitor_info) &&
        EnumDisplaySettingsW(monitor_info.szDevice, ENUM_CURRENT_SETTINGS, &display_mode)) {
        fprintf(stderr, "[wgc] display %d: %ls %lux%lu @%luHz\n", display_index,
                monitor_info.szDevice, display_mode.dmPelsWidth, display_mode.dmPelsHeight,
                display_mode.dmDisplayFrequency);
    }

    // 4. Get IGraphicsCaptureItemInterop activation factory
    static const wchar_t item_class_name[] =
        L"Windows.Graphics.Capture.GraphicsCaptureItem";
    HSTRING item_class = nullptr;
    hr = WindowsCreateString(item_class_name,
                             ARRAYSIZE(item_class_name) - 1, &item_class);
    if (FAILED(hr)) {
        return wgc_create_failed(cap, "WindowsCreateString item", hr);
    }
    ComPtr<IActivationFactory> item_factory;
    hr = RoGetActivationFactory(item_class, IID_PPV_ARGS(&item_factory));
    WindowsDeleteString(item_class);
    if (FAILED(hr)) {
        return wgc_create_failed(cap, "GraphicsCaptureItem factory", hr);
    }

    ComPtr<IGraphicsCaptureItemInterop> item_interop;
    hr = item_factory.As(&item_interop);
    if (FAILED(hr)) {
        return wgc_create_failed(cap, "GraphicsCaptureItem interop", hr);
    }

    // 5. Create GraphicsCaptureItem for monitor
    hr = item_interop->CreateForMonitor(hmonitor, IID_PPV_ARGS(&cap->capture_item));
    if (FAILED(hr)) {
        return wgc_create_failed(cap, "CreateForMonitor", hr);
    }

    // 6. Get size of capture item
    SizeInt32 item_size = {};
    cap->capture_item->get_Size(&item_size);
    if (item_size.Width == 0 || item_size.Height == 0) {
        MONITORINFOEXW mi = {};
        mi.cbSize = sizeof(mi);
        GetMonitorInfoW(hmonitor, &mi);
        item_size.Width = mi.rcMonitor.right - mi.rcMonitor.left;
        item_size.Height = mi.rcMonitor.bottom - mi.rcMonitor.top;
    }

    // 7. Get frame pool activation factory
    static const wchar_t pool_class_name[] =
        L"Windows.Graphics.Capture.Direct3D11CaptureFramePool";
    HSTRING pool_class = nullptr;
    hr = WindowsCreateString(pool_class_name,
                             ARRAYSIZE(pool_class_name) - 1, &pool_class);
    if (FAILED(hr)) {
        return wgc_create_failed(cap, "WindowsCreateString frame pool", hr);
    }
    ComPtr<IActivationFactory> frame_pool_factory;
    hr = RoGetActivationFactory(pool_class, IID_PPV_ARGS(&frame_pool_factory));
    WindowsDeleteString(pool_class);
    if (FAILED(hr)) {
        return wgc_create_failed(cap, "FramePool factory", hr);
    }

    ComPtr<IDirect3D11CaptureFramePoolStatics2> frame_pool_statics;
    hr = frame_pool_factory.As(&frame_pool_statics);
    if (FAILED(hr)) {
        return wgc_create_failed(cap, "FramePool statics", hr);
    }

    // 8. Create frame pool — use CreateFreeThreaded so we don't need a
    //    DispatcherQueue on the calling thread. Available on Win10 1903+.
    hr = frame_pool_statics->CreateFreeThreaded(
        cap->winrt_device.Get(),
        DirectXPixelFormat_B8G8R8A8UIntNormalized,
        2,
        item_size,
        &cap->frame_pool);
    if (FAILED(hr)) {
        return wgc_create_failed(cap, "CreateFreeThreaded", hr);
    }

    // 9. Create capture session
    hr = cap->frame_pool->CreateCaptureSession(cap->capture_item.Get(), &cap->session);
    if (FAILED(hr)) {
        return wgc_create_failed(cap, "CreateCaptureSession", hr);
    }

    return cap;
}

int wgc_start(WgcCapture* cap, int fps) {
    if (!cap || !cap->session) return -1;

    ComPtr<IGraphicsCaptureSession5> pacing;
    if (fps > 0 && SUCCEEDED(cap->session.As(&pacing))) {
        CaptureTimeSpan minimum_interval = {10000000LL / fps};
        HRESULT pacing_hr = pacing->put_MinUpdateInterval(minimum_interval);
        if (FAILED(pacing_hr)) {
            fprintf(stderr, "[wgc] update interval rejected (0x%08lx)\n", (unsigned long)pacing_hr);
        }
        if (SUCCEEDED(pacing->get_MinUpdateInterval(&minimum_interval))) {
            fprintf(stderr, "[wgc] minimum update interval: %.2fms\n", minimum_interval.Duration / 10000.0);
        }
    }

    HRESULT hr = cap->session->StartCapture();
    if (FAILED(hr)) return -1;

    cap->running = true;

    cap->capture_thread = std::thread([cap]() {
        FrameWait frame_wait;
        char profile_flag[2] = {};
        bool profile = GetEnvironmentVariableA("RELAYNET_MEDIA_PROFILE", profile_flag, sizeof(profile_flag)) && profile_flag[0] == '1';
        int64_t read_ns = 0, callback_ns = 0;
        auto profile_since = std::chrono::steady_clock::now();
        int frame_count = 0;
        while (cap->running) {
            ComPtr<IDirect3D11CaptureFrame> frame;
            HRESULT hr = cap->frame_pool->TryGetNextFrame(&frame);
            if (FAILED(hr) || !frame) {
                frame_wait.wait();
                continue;
            }

            frame_count++;
            if (frame_count == 1) {
                fprintf(stderr, "[wgc] first frame received\n");
                fflush(stderr);
            }

            // Get the Direct3D surface from the frame
            ComPtr<IDirect3DSurface> d3d_surface;
            if (FAILED(frame->get_Surface(&d3d_surface)) || !d3d_surface) {
                continue;
            }

            // WinRT IDirect3DSurface cannot be QI'd directly for IDXGISurface.
            // We must go through IDirect3DDxgiInterfaceAccess::GetInterface.
            ComPtr<IDirect3DDxgiInterfaceAccess> dxgi_access;
            if (FAILED(d3d_surface.As(&dxgi_access))) {
                fprintf(stderr, "[wgc] QI for IDirect3DDxgiInterfaceAccess failed\n");
                continue;
            }

            ComPtr<ID3D11Texture2D> src_texture;
            if (FAILED(dxgi_access->GetInterface(IID_PPV_ARGS(&src_texture)))) {
                fprintf(stderr, "[wgc] GetInterface for ID3D11Texture2D failed\n");
                continue;
            }

            D3D11_TEXTURE2D_DESC src_desc = {};
            src_texture->GetDesc(&src_desc);

            int w = static_cast<int>(src_desc.Width);
            int h = static_cast<int>(src_desc.Height);

            // Reuse the staging texture; only recreate when dimensions change.
            if (!cap->staging_tex || cap->staging_w != w || cap->staging_h != h) {
                D3D11_TEXTURE2D_DESC tex_desc = {};
                tex_desc.Width = src_desc.Width;
                tex_desc.Height = src_desc.Height;
                tex_desc.MipLevels = 1;
                tex_desc.ArraySize = 1;
                tex_desc.Format = src_desc.Format;
                tex_desc.SampleDesc.Count = 1;
                tex_desc.Usage = D3D11_USAGE_STAGING;
                tex_desc.CPUAccessFlags = D3D11_CPU_ACCESS_READ;

                cap->staging_tex.Reset();
                if (FAILED(cap->d3d_device->CreateTexture2D(&tex_desc, nullptr, &cap->staging_tex))) {
                    fprintf(stderr, "[wgc] CreateTexture2D (staging) failed\n");
                    continue;
                }
                cap->staging_w = w;
                cap->staging_h = h;
            }

            auto read_start = std::chrono::steady_clock::now();
            cap->d3d_context->CopyResource(cap->staging_tex.Get(), src_texture.Get());

            D3D11_MAPPED_SUBRESOURCE mapped = {};
            if (SUCCEEDED(cap->d3d_context->Map(cap->staging_tex.Get(), 0, D3D11_MAP_READ, 0, &mapped))) {
                if (cap->callback) {
                    auto callback_start = std::chrono::steady_clock::now();
                    if (profile) read_ns += std::chrono::duration_cast<std::chrono::nanoseconds>(callback_start - read_start).count();
                    UINT64 frame_time = 0;
                    LARGE_INTEGER counter, frequency;
                    int64_t age_ns = 0;
                    if (SUCCEEDED(frame->get_SystemRelativeTime(&frame_time)) &&
                        QueryPerformanceCounter(&counter) && QueryPerformanceFrequency(&frequency)) {
                        int64_t now_100ns = (int64_t)((long double)counter.QuadPart * 10000000 / frequency.QuadPart);
                        if (now_100ns >= (int64_t)frame_time) age_ns = (now_100ns - (int64_t)frame_time) * 100;
                    }
                    // The callback copies into its bounded raw-frame pool;
                    // avoid copying the entire BGRA image a second time here.
                    cap->callback(static_cast<const uint8_t*>(mapped.pData), w, h,
                                  (int)mapped.RowPitch, age_ns, cap->userdata);
                    if (profile) callback_ns += std::chrono::duration_cast<std::chrono::nanoseconds>(std::chrono::steady_clock::now() - callback_start).count();
                }

                cap->d3d_context->Unmap(cap->staging_tex.Get(), 0);
            }
            if (profile && frame_count % 120 == 0) {
                auto now = std::chrono::steady_clock::now();
                double seconds = std::chrono::duration<double>(now - profile_since).count();
                fprintf(stderr, "[wgc] profile: capture=%.1ffps readback=%.2fms callback=%.2fms\n", 120 / seconds, read_ns / 120e6, callback_ns / 120e6);
                read_ns = callback_ns = 0;
                profile_since = now;
            }
        }
    });

    return 0;
}

void wgc_stop(WgcCapture* cap) {
    if (!cap) return;
    cap->running = false;
    if (cap->capture_thread.joinable()) {
        cap->capture_thread.join();
    }
    if (cap->session) {
        ComPtr<IClosable> closable;
        if (SUCCEEDED(cap->session.As(&closable))) {
            closable->Close();
        }
    }
}

void wgc_destroy(WgcCapture* cap) {
    if (!cap) return;
    wgc_stop(cap);
    if (cap->frame_pool) {
        ComPtr<IClosable> closable;
        if (SUCCEEDED(cap->frame_pool.As(&closable))) {
            closable->Close();
        }
    }
    cap->session.Reset();
    cap->frame_pool.Reset();
    cap->capture_item.Reset();
    cap->winrt_device.Reset();
    cap->d3d_context.Reset();
    cap->d3d_device.Reset();
    delete cap;
    RoUninitialize();
}
