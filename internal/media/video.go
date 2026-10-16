package media

import (
	"bytes"
	"fmt"
	"image"
	"image/jpeg"
	"sync"
	"time"

	"github.com/kbinani/screenshot"
)

type VideoQuality struct {
	Width   int
	Height  int
	FPS     int
	Bitrate int
}

var VideoQualities = map[string]VideoQuality{
	"720p":  {Width: 1280, Height: 720, FPS: 30, Bitrate: 2500},
	"1080p": {Width: 1920, Height: 1080, FPS: 30, Bitrate: 4500},
	"1440p": {Width: 2560, Height: 1440, FPS: 30, Bitrate: 6500},
	"2160p": {Width: 3840, Height: 2160, FPS: 30, Bitrate: 8500},
}

// VideoFrameType values must stay in sync with proto.VideoFrame* constants.
const (
	FrameTypeJPEG    byte = 0 // JPEG frame (legacy / fallback path)
	FrameTypeH264IDR byte = 1 // H.264 IDR keyframe
	FrameTypeH264P   byte = 2 // H.264 P-frame
)

// VideoFrame is one encoded frame travelling from the capturer to the tunnel.
type VideoFrame struct {
	Data           []byte // encoded payload (JPEG bytes or H.264 Annex-B)
	Type           byte   // one of FrameType*
	SourceID       uint32 // room member that sent this frame (zero for local capture)
	StreamID       uint64
	Sequence       uint32
	TimestampNS    int64 // monotonic capture timestamp, session-relative on the wire
	HasAudio       bool
	Discontinuity  bool
	SubscriptionID uint64
}

type VideoCapturer struct {
	displayIndex int
	quality      VideoQuality
	method       CaptureMethod
	running      bool
	frameChan    chan VideoFrame
	stopChan     chan struct{}
	callback     func(VideoFrame)
	mu           sync.Mutex
	wg           sync.WaitGroup
	wgc          *WgcCapturer // non-nil when using WGC
	h264         *H264Encoder // DXGI path encoder (created on Start)
}

func NewVideoCapturer(displayIndex int, quality VideoQuality, method CaptureMethod, callback func(VideoFrame)) *VideoCapturer {
	vc := &VideoCapturer{
		displayIndex: displayIndex,
		quality:      quality,
		method:       ResolveMethod(method),
		callback:     callback,
		frameChan:    make(chan VideoFrame, 4),
		stopChan:     make(chan struct{}),
	}
	return vc
}

func (vc *VideoCapturer) Start() {
	vc.mu.Lock()
	vc.running = true
	method := vc.method
	vc.mu.Unlock()

	// consumer loop: exits automatically when frameChan is closed.
	go func() {
		for frame := range vc.frameChan {
			vc.callback(frame)
		}
	}()

	if method == CaptureMethodWGC {
		vc.startWGC()
		return
	}
	vc.startDXGI()
}

// startDXGI runs the ticker-based DXGI/BitBlt capture loop.
func (vc *VideoCapturer) startDXGI() {
	// Also runs after WGC startup fails, so initialize the encoder here rather
	// than only for an explicitly selected DXGI capture method.
	bounds := screenshot.GetDisplayBounds(vc.displayIndex)
	if bounds.Dx() > 0 && bounds.Dy() > 0 {
		if enc, err := NewH264Encoder(bounds.Dx(), bounds.Dy(),
			vc.quality.Width, vc.quality.Height,
			vc.quality.FPS, vc.quality.Bitrate); err == nil {
			vc.mu.Lock()
			vc.h264 = enc
			vc.mu.Unlock()
		} else {
			fmt.Printf("h264: DXGI encoder unavailable (%v), JPEG fallback\n", err)
		}
	}
	vc.wg.Add(1)
	go func() {
		defer vc.wg.Done()
		ticker := time.NewTicker(time.Second / time.Duration(vc.quality.FPS))
		defer ticker.Stop()

		for {
			vc.mu.Lock()
			running := vc.running
			vc.mu.Unlock()
			if !running {
				break
			}

			capturePTS := CaptureTimestamp()
			img, err := screenshot.CaptureDisplay(vc.displayIndex)
			if err != nil {
				fmt.Printf("screenshot capture failed (display=%d): %v\n", vc.displayIndex, err)
				time.Sleep(50 * time.Millisecond)
				continue
			}

			// H.264 fast path: the encoder scales + encodes in one call.
			frame, encoded := vc.encodeH264(img, capturePTS)
			if !encoded {
				// Fallback: scale + JPEG (encoder unavailable or errored).
				scaled := scaleImage(img, vc.quality.Width, vc.quality.Height)
				frame = VideoFrame{Data: encodeJPEG(scaled), Type: FrameTypeJPEG, TimestampNS: capturePTS}
			}

			if len(frame.Data) > 0 {
				vc.mu.Lock()
				running = vc.running
				vc.mu.Unlock()
				if !running {
					break
				}
				// Backpressure before encoding the next frame. Dropping an
				// already-encoded H.264 frame breaks the decoder reference chain.
				select {
				case vc.frameChan <- frame:
				case <-vc.stopChan:
					return
				}
			}

			<-ticker.C
		}
	}()
}

// encodeH264 encodes a captured RGBA image through the DXGI H.264 encoder.
// Returns ok=false when no encoder is active (caller falls back to JPEG).
func (vc *VideoCapturer) encodeH264(img image.Image, capturePTS int64) (VideoFrame, bool) {
	vc.mu.Lock()
	enc := vc.h264
	vc.mu.Unlock()
	if enc == nil {
		return VideoFrame{}, false
	}
	rgba, ok := img.(*image.RGBA)
	if !ok || rgba == nil {
		return VideoFrame{}, false
	}
	data, keyframe, outputPTS, err := enc.SendAt(rgba.Pix, rgba.Stride, true, capturePTS)
	if err != nil {
		fmt.Printf("h264: DXGI encode failed (%v), disabling\n", err)
		enc.Close()
		vc.mu.Lock()
		vc.h264 = nil
		vc.mu.Unlock()
		return VideoFrame{}, false
	}
	if len(data) == 0 {
		return VideoFrame{}, false // encoder dropped the frame (busy)
	}
	typ := FrameTypeH264P
	if keyframe {
		typ = FrameTypeH264IDR
	}
	return VideoFrame{Data: data, Type: typ, TimestampNS: outputPTS}, true
}

// startWGC uses Windows Graphics Capture. Frames are pushed by the native
// capture thread directly into frameChan.
func (vc *VideoCapturer) startWGC() {
	wgc := NewWgcCapturer(vc.displayIndex, vc.quality, func(frame VideoFrame) {
		vc.mu.Lock()
		running := vc.running
		vc.mu.Unlock()
		if !running {
			return
		}
		// Preserve every encoded frame; the bounded channel naturally slows
		// capture when the network sender is pacing a large frame.
		select {
		case vc.frameChan <- frame:
		case <-vc.stopChan:
		}
	})
	if wgc == nil {
		fmt.Printf("wgc: not supported, falling back to DXGI\n")
		vc.startDXGI()
		return
	}
	if err := wgc.Start(); err != nil {
		fmt.Printf("wgc: start failed (%v), falling back to DXGI\n", err)
		vc.startDXGI()
		return
	}
	vc.mu.Lock()
	vc.wgc = wgc
	vc.mu.Unlock()
}

func (vc *VideoCapturer) Stop() {
	vc.mu.Lock()
	if vc.running {
		close(vc.stopChan)
	}
	vc.running = false
	wgc := vc.wgc
	enc := vc.h264
	vc.h264 = nil
	vc.mu.Unlock()
	if wgc != nil {
		wgc.Stop()
	}
	vc.wg.Wait() // Native SendAt may still own enc until capture returns.
	if enc != nil {
		enc.Close()
	}
}

func (vc *VideoCapturer) Close() {
	vc.Stop()
	vc.wg.Wait() // wait for capture loop to exit before closing the channel
	close(vc.frameChan)
}

// Method returns the resolved capture method (Auto is replaced by WGC/DXGI).
func (vc *VideoCapturer) Method() CaptureMethod {
	vc.mu.Lock()
	defer vc.mu.Unlock()
	return vc.method
}

// AddOutput reuses WGC readback; the fallback uses the existing capturer.
// Call after Start, and close outputs before closing the primary capturer.
func (vc *VideoCapturer) AddOutput(q VideoQuality, callback func(VideoFrame)) func() {
	vc.mu.Lock()
	wgc := vc.wgc
	vc.mu.Unlock()
	if wgc != nil {
		return wgc.AddOutput(q, callback)
	}
	output := NewVideoCapturer(vc.displayIndex, q, CaptureMethodDXGI, callback)
	output.Start()
	var once sync.Once
	return func() { once.Do(output.Close) }
}

func (vc *VideoCapturer) SetQuality(q VideoQuality) {
	vc.mu.Lock()
	vc.quality = q
	vc.mu.Unlock()
}

func ListDisplays() []string {
	count := screenshot.NumActiveDisplays()
	result := make([]string, count)
	for i := 0; i < count; i++ {
		bounds := screenshot.GetDisplayBounds(i)
		result[i] = displayName(i, bounds)
	}
	return result
}

func displayName(index int, bounds image.Rectangle) string {
	w := bounds.Dx()
	h := bounds.Dy()
	return imageToString(index, w, h)
}

func imageToString(index, w, h int) string {
	return displayString(index, w, h)
}

func displayString(index, w, h int) string {
	return stringFromParts(index, w, h)
}

func stringFromParts(index, w, h int) string {
	result := make([]byte, 0, 32)
	result = appendInt(result, index)
	result = append(result, ' ')
	result = append(result, '(')
	result = appendInt(result, w)
	result = append(result, 'x')
	result = appendInt(result, h)
	result = append(result, ')')
	return string(result)
}

func appendInt(b []byte, v int) []byte {
	if v < 0 {
		b = append(b, '-')
		v = -v
	}
	return appendUint(b, uint(v))
}

func appendUint(b []byte, v uint) []byte {
	if v == 0 {
		return append(b, '0')
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte(v%10) + '0'
		v /= 10
	}
	return append(b, buf[i:]...)
}

func scaleImage(img image.Image, width, height int) image.Image {
	srcBounds := img.Bounds()
	srcW := srcBounds.Dx()
	srcH := srcBounds.Dy()

	if srcW == width && srcH == height {
		return img
	}

	dst := image.NewRGBA(image.Rect(0, 0, width, height))
	src := img.(*image.RGBA)

	// Integer nearest-neighbor: srcX = x * srcW / dstW. Precompute with the
	// destination dimension in the divisor to avoid float per-pixel.
	for y := 0; y < height; y++ {
		srcY := y * srcH / height
		if srcY >= srcH {
			srcY = srcH - 1
		}
		srcRow := srcY * src.Stride
		dstRow := y * dst.Stride
		for x := 0; x < width; x++ {
			srcX := x * srcW / width
			if srcX >= srcW {
				srcX = srcW - 1
			}
			srcIdx := srcRow + srcX*4
			dstIdx := dstRow + x*4
			copy(dst.Pix[dstIdx:dstIdx+4], src.Pix[srcIdx:srcIdx+4])
		}
	}
	return dst
}

// scaleBGRAtoRGBA scales a BGRA buffer to an *image.RGBA in a single pass,
// performing both the B↔R channel swap and nearest-neighbor downscale at once.
// Used by the WGC capture path to avoid a separate BGRA→RGBA copy before
// scaling. srcStride is the byte stride of the source buffer.
func scaleBGRAtoRGBA(src []byte, srcW, srcH, srcStride, dstW, dstH int) *image.RGBA {
	if srcW == dstW && srcH == dstH {
		// No scaling needed: just swap B and R channels into a new RGBA buffer.
		dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
		dstPix := dst.Pix
		n := dstW * dstH
		for i := 0; i < n; i++ {
			s := src[i*4 : i*4+4]
			d := dstPix[i*4 : i*4+4]
			d[0] = s[2] // R
			d[1] = s[1] // G
			d[2] = s[0] // B
			d[3] = s[3] // A
		}
		return dst
	}

	dst := image.NewRGBA(image.Rect(0, 0, dstW, dstH))
	dstPix := dst.Pix
	dstStride := dst.Stride
	for y := 0; y < dstH; y++ {
		srcY := y * srcH / dstH
		if srcY >= srcH {
			srcY = srcH - 1
		}
		srcRow := srcY * srcStride
		dstRow := y * dstStride
		for x := 0; x < dstW; x++ {
			srcX := x * srcW / dstW
			if srcX >= srcW {
				srcX = srcW - 1
			}
			s := src[srcRow+srcX*4:]
			d := dstPix[dstRow+x*4:]
			d[0] = s[2] // R
			d[1] = s[1] // G
			d[2] = s[0] // B
			d[3] = s[3] // A
		}
	}
	return dst
}

// encodeJPEG compresses an image to JPEG. It uses libjpeg-turbo (via CGO)
// when the image is *image.RGBA, falling back to the Go stdlib encoder
// otherwise. libjpeg-turbo is ~10x faster than stdlib, which is required to
// hit 120 FPS at 720p/1080p.
func encodeJPEG(img image.Image) []byte {
	if rgba, ok := img.(*image.RGBA); ok {
		if data, err := TurboEncode(rgba, 80); err == nil {
			return data
		}
	}
	buf := &bytes.Buffer{}
	jpeg.Encode(buf, img, &jpeg.Options{Quality: 80})
	return buf.Bytes()
}

func appendSOI(b []byte) []byte {
	return append(b, 0xFF, 0xD8)
}

func appendJPEGHeader(b []byte, width, height int) []byte {
	b = append(b, 0xFF, 0xC0)
	b = append(b, 0x00, 0x11)
	b = append(b, 0x08)
	b = appendBE16(b, uint16(height))
	b = appendBE16(b, uint16(width))
	b = append(b, 0x03)
	b = append(b, 0x01, 0x22, 0x00)
	b = append(b, 0x02, 0x11, 0x01)
	b = append(b, 0x03, 0x11, 0x01)
	return b
}

func appendBE16(b []byte, v uint16) []byte {
	return append(b, byte(v>>8), byte(v))
}

func appendScanline(b []byte, data []byte, width, height int) []byte {
	b = append(b, 0xFF, 0xDA)
	b = append(b, 0x00, 0x0C)
	b = append(b, 0x03)
	b = appendBE16(b, uint16(width))
	b = appendBE16(b, uint16(height))
	b = append(b, 0x00)

	blockSize := 8
	for y := 0; y < height; y += blockSize {
		b = append(b, 0x00)
		for x := 0; x < width; x += blockSize {
			for by := 0; by < blockSize && y+by < height; by++ {
				for bx := 0; bx < blockSize && x+bx < width; bx++ {
					idx := (y+by)*width + (x + bx)
					if idx < len(data) {
						b = append(b, data[idx])
					} else {
						b = append(b, 0)
					}
				}
			}
		}
	}

	return b
}

func imageToYCbCr(img image.Image) *image.YCbCr {
	bounds := img.Bounds()
	width := bounds.Dx()
	height := bounds.Dy()

	ycbcr := image.NewYCbCr(bounds, image.YCbCrSubsampleRatio420)

	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			ycbcr.Y[y*width+x] = rgbToY(uint8(r>>8), uint8(g>>8), uint8(b>>8))
			if x%2 == 0 && y%2 == 0 {
				ycbcr.Cb[(y/2)*(width/2)+(x/2)] = rgbToCb(uint8(r>>8), uint8(g>>8), uint8(b>>8))
				ycbcr.Cr[(y/2)*(width/2)+(x/2)] = rgbToCr(uint8(r>>8), uint8(g>>8), uint8(b>>8))
			}
		}
	}

	return ycbcr
}

func rgbToY(r, g, b uint8) uint8 {
	return uint8((66*int(r)+129*int(g)+25*int(b)+128)>>8) + 16
}

func rgbToCb(r, g, b uint8) uint8 {
	return uint8((-38*int(r)-74*int(g)+112*int(b)+128)>>8) + 128
}

func rgbToCr(r, g, b uint8) uint8 {
	return uint8((112*int(r)-94*int(g)-18*int(b)+128)>>8) + 128
}

type VideoRenderer struct {
	running     bool
	mu          sync.RWMutex
	callback    func(VideoFrame)
	latestFrame VideoFrame
	frameSeq    uint64
	history     []VideoFrame
}

const videoHistorySize = 120

func NewVideoRenderer(callback func(VideoFrame)) *VideoRenderer {
	return &VideoRenderer{
		callback: callback,
		history:  make([]VideoFrame, 0, videoHistorySize),
	}
}

func (vr *VideoRenderer) Start() {
	vr.mu.Lock()
	vr.running = true
	vr.mu.Unlock()
}

func (vr *VideoRenderer) Stop() {
	vr.mu.Lock()
	vr.running = false
	vr.mu.Unlock()
}

func (vr *VideoRenderer) Close() {
	vr.mu.Lock()
	vr.running = false
	vr.history = nil
	vr.latestFrame = VideoFrame{}
	vr.mu.Unlock()
}

// Render stores every received compressed frame in order without blocking the
// network loop on decoding or dropping a frame that H.264 may reference later.
func (vr *VideoRenderer) Render(frame VideoFrame) {
	vr.mu.Lock()
	if !vr.running {
		vr.mu.Unlock()
		return
	}
	vr.frameSeq++
	vr.latestFrame = frame
	if len(vr.history) == videoHistorySize {
		copy(vr.history, vr.history[1:])
		vr.history[len(vr.history)-1] = frame
	} else {
		vr.history = append(vr.history, frame)
	}
	callback := vr.callback
	vr.mu.Unlock()
	if callback != nil {
		callback(frame)
	}
}

// FramesSince returns compressed frames in receive order. missed reports that
// the caller fell behind the bounded history and must restart its H.264 decoder.
func (vr *VideoRenderer) FramesSince(lastSeq uint64) (seq uint64, frames []VideoFrame, missed bool) {
	vr.mu.RLock()
	defer vr.mu.RUnlock()
	seq = vr.frameSeq
	if len(vr.history) == 0 || lastSeq >= seq {
		return seq, nil, false
	}
	firstSeq := seq - uint64(len(vr.history)) + 1
	startSeq := lastSeq + 1
	if startSeq < firstSeq {
		missed = lastSeq != 0
		startSeq = firstSeq
	}
	frames = append([]VideoFrame(nil), vr.history[int(startSeq-firstSeq):]...)
	return seq, frames, missed
}

// LatestFrame returns the most recently received frame, or a zero VideoFrame.
func (vr *VideoRenderer) LatestFrame() VideoFrame {
	vr.mu.RLock()
	defer vr.mu.RUnlock()
	return vr.latestFrame
}

// LatestFrameSeq returns the sequence number of the last rendered frame.
// The UI can compare this to detect when a new frame is available without
// re-decoding the same frame.
func (vr *VideoRenderer) LatestFrameSeq() (uint64, VideoFrame) {
	vr.mu.RLock()
	defer vr.mu.RUnlock()
	return vr.frameSeq, vr.latestFrame
}

func getDisplayInfo(index int) (string, image.Rectangle) {
	bounds := screenshot.GetDisplayBounds(index)
	return displayName(index, bounds), bounds
}
