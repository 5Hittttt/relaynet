package tunnel

import (
	"time"

	"relaynet/internal/media"
	"relaynet/internal/proto"
)

func (c *Client) startScreenAudio(id uint64, startPTS int64, quality media.ScreenAudioQuality) (*media.AudioCapturer, chan struct{}, chan struct{}, error) {
	rate, channels := quality.Format()
	packetizer := media.NewScreenAudioPacketizer(quality)
	chunks := make(chan media.ScreenAudioChunk, 120)
	stop, done := make(chan struct{}), make(chan struct{})
	capturer, err := media.NewScreenAudioCapturer(quality, func(samples []float32) {
		firstPTS := media.CaptureTimestamp() - startPTS - int64(len(samples)/channels)*int64(time.Second)/int64(rate)
		for _, chunk := range packetizer.Push(samples, firstPTS) {
			select {
			case chunks <- chunk:
			case <-stop:
				return
			default:
			} // Missing intervals retain their timestamps.
		}
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if err = capturer.Start(); err != nil {
		capturer.Close()
		return nil, nil, nil, err
	}
	go func() {
		defer close(done)
		ticker := time.NewTicker(media.ScreenAudioPacketDuration)
		defer ticker.Stop()
		var sequence uint32
		var nextPTS int64
		silence := make([]float32, rate*channels/200)
		send := func(pts int64, data []byte) {
			sequence++
			payload := proto.EncodeScreenPayload(proto.ScreenPayload{Kind: proto.ScreenAudio, HasAudio: true, StreamID: id, Sequence: sequence, TimestampNS: pts}, data)
			c.mu.Lock()
			conn, key, source := c.conn, c.dataKey, c.clientID
			c.mu.Unlock()
			if conn == nil {
				return
			}
			packet, err := proto.EncodeSystemAudioData(source, proto.BroadcastID, key, payload)
			if err == nil {
				conn.Write(packet)
			}
		}
		for {
			select {
			case <-stop:
				send(max(media.CaptureTimestamp()-startPTS, 0), []byte{0})
				return
			case chunk := <-chunks:
				if chunk.TimestampNS+int64(media.ScreenAudioPacketDuration) <= nextPTS {
					continue
				}
				send(chunk.TimestampNS, media.EncodeScreenAudio(quality, chunk.Samples))
				nextPTS = chunk.TimestampNS + int64(media.ScreenAudioPacketDuration)
			case <-ticker.C:
				// WASAPI loopback may stop callbacks on a silent desktop. Keep
				// a continuous audio clock with timestamped silence in that case.
				limit := media.CaptureTimestamp() - startPTS - int64(20*time.Millisecond)
				if len(chunks) != 0 {
					continue
				}
				if limit-nextPTS > int64(500*time.Millisecond) {
					nextPTS = max(limit-int64(120*time.Millisecond), 0)
				}
				for nextPTS+int64(media.ScreenAudioPacketDuration) <= limit {
					send(nextPTS, media.EncodeScreenAudio(quality, silence))
					nextPTS += int64(media.ScreenAudioPacketDuration)
				}
			}
		}
	}()
	c.log("screen audio started: quality=%d rate=%d channels=%d", quality, rate, channels)
	return capturer, stop, done, nil
}

// SelectScreenSource switches the audio device and clock together, so packets
// from another room member cannot enter the selected screen's playback queue.
func (c *Client) SelectScreenSource(source uint32) {
	c.screenSelectMu.Lock()
	defer c.screenSelectMu.Unlock()
	c.mu.Lock()
	if c.screenSource == source {
		c.mu.Unlock()
		return
	}
	old := c.screenAudioPlayer
	oldSource := c.screenSource
	c.screenAudioPlayer = nil
	c.screenSource = source
	c.screenSubscriptionID++
	timeline := media.NewScreenTimeline()
	c.screenTimeline = timeline
	connected, own := c.conn != nil, c.clientID
	c.mu.Unlock()
	if oldSource != 0 && oldSource != source {
		c.sendScreenSubscription(oldSource, false)
	}
	c.sendScreenSubscription(source, true)
	if old != nil {
		old.Close()
	}
	if source == 0 || source == own || !connected {
		return
	}
	player, err := media.NewScreenAudioPlayer(timeline)
	if err != nil {
		c.log("screen audio playback unavailable: %v", err)
		return
	}
	c.mu.Lock()
	if c.conn == nil || c.screenSource != source {
		c.mu.Unlock()
		player.Close()
		return
	}
	c.screenAudioPlayer = player
	c.mu.Unlock()
}

func (c *Client) ScreenPosition(frame media.VideoFrame) (int64, bool) {
	if frame.StreamID == 0 {
		return frame.TimestampNS, true
	}
	c.mu.Lock()
	timeline, source, player := c.screenTimeline, c.screenSource, c.screenAudioPlayer
	c.mu.Unlock()
	if timeline == nil || source != frame.SourceID {
		return 0, false
	}
	if player == nil {
		frame.HasAudio = false
	}
	timeline.ObserveVideo(frame, time.Now())
	return timeline.Position(frame.StreamID, time.Now())
}

func (c *Client) renderScreenVideo(source uint32, typ byte, payload []byte) {
	meta, data, marked, err := proto.DecodeScreenPayload(payload)
	if err != nil || (marked && meta.Kind != proto.ScreenVideo) {
		return
	}
	frame := media.VideoFrame{Data: data, Type: typ, SourceID: source}
	if marked {
		frame.StreamID, frame.Sequence, frame.TimestampNS, frame.HasAudio = meta.StreamID, meta.Sequence, meta.TimestampNS, meta.HasAudio
		frame.SubscriptionID = meta.SubscriptionID
	}
	c.mu.Lock()
	renderer, timeline, selected, player := c.videoRenderer, c.screenTimeline, c.screenSource, c.screenAudioPlayer
	requestID := c.screenSubscriptionID
	if !c.screenSubscriptionSources[source] {
		requestID = 0
	}
	c.mu.Unlock()
	if selected != 0 && (source != selected || frame.SubscriptionID != requestID) {
		return
	}
	if timeline != nil && selected == source && marked {
		observed := frame
		if player == nil {
			observed.HasAudio = false
		}
		timeline.ObserveVideo(observed, time.Now())
	}
	if renderer != nil {
		renderer.Render(frame)
	}
}

func (c *Client) receiveScreenAudio(source uint32, meta proto.ScreenPayload, data []byte) {
	c.mu.Lock()
	selected, timeline, player := c.screenSource, c.screenTimeline, c.screenAudioPlayer
	c.mu.Unlock()
	if source != selected || timeline == nil || player == nil {
		return
	}
	if len(data) == 1 && data[0] == 0 {
		timeline.Stop(meta.StreamID)
		return
	}
	samples, err := media.DecodeScreenAudio(data)
	if err != nil {
		return
	}
	volume := c.GetPeerVolume(source)
	if volume != 1 {
		for i := range samples {
			samples[i] *= volume
		}
	}
	timeline.PushAudio(meta.StreamID, meta.TimestampNS, samples, time.Now())
}

func (c *Client) ScreenVideoDecoded(frame media.VideoFrame) {
	if frame.StreamID == 0 {
		return
	}
	c.mu.Lock()
	timeline, source, player := c.screenTimeline, c.screenSource, c.screenAudioPlayer
	c.mu.Unlock()
	if timeline == nil || source != frame.SourceID {
		return
	}
	if player == nil {
		frame.HasAudio = false
	}
	timeline.ObserveVideo(frame, time.Now())
	timeline.VideoDecoded(frame.StreamID, frame.TimestampNS)
}

func (c *Client) ScreenDecodeLimit(frame media.VideoFrame) int64 {
	if frame.StreamID == 0 {
		return frame.TimestampNS
	}
	c.mu.Lock()
	timeline := c.screenTimeline
	c.mu.Unlock()
	if timeline == nil {
		return frame.TimestampNS
	}
	return timeline.DecodeLimit(frame.StreamID, time.Now())
}

func (c *Client) ScreenDiscardBefore(frame media.VideoFrame) int64 {
	if frame.StreamID == 0 {
		return 0
	}
	c.mu.Lock()
	timeline, source := c.screenTimeline, c.screenSource
	c.mu.Unlock()
	if timeline == nil || source != frame.SourceID {
		return 0
	}
	return timeline.VideoDiscardBefore(frame.StreamID, time.Now())
}
