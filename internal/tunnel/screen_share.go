package tunnel

import (
	"encoding/binary"
	"sync"
	"time"

	"relaynet/internal/crypto"
	"relaynet/internal/media"
	"relaynet/internal/proto"
)

func screenPlaybackQuality(source media.VideoQuality, request proto.ScreenSubscription) media.VideoQuality {
	q := source
	heights := []int{0, 720, 1080, 1440, 2160}
	if request.Resolution > 0 && request.Resolution < 5 && heights[request.Resolution] < q.Height {
		q.Height = heights[request.Resolution]
		q.Width = q.Height * source.Width / source.Height
		q.Width -= q.Width % 2
	}
	if request.FPS > 0 {
		q.FPS = min(q.FPS, int(request.FPS))
	}
	if q != source {
		q.Bitrate = max(500, source.Bitrate*q.Height/source.Height*q.FPS/source.FPS)
		q.Bitrate = min(q.Bitrate, source.Bitrate)
	}
	return q
}

type screenViewerRequest struct {
	request proto.ScreenSubscription
	seen    time.Time
}

type screenShareSession struct {
	client     *Client
	base       *media.VideoCapturer
	quality    media.VideoQuality
	streamID   uint64
	startPTS   int64
	hasAudio   bool
	mu         sync.Mutex
	viewers    map[uint32]screenViewerRequest
	negotiated map[uint32]bool
	stop       chan struct{}
	done       chan struct{}
	wake       chan struct{}
	closing    bool
}

func (s *screenShareSession) request(peer uint32, request proto.ScreenSubscription) {
	s.mu.Lock()
	if !s.closing && request.RequestID >= s.viewers[peer].request.RequestID {
		s.viewers[peer] = screenViewerRequest{request, time.Now()}
		if s.negotiated == nil {
			s.negotiated = make(map[uint32]bool)
		}
		s.negotiated[peer] = !request.Legacy
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *screenShareSession) targets(q media.VideoQuality) map[uint32]uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	targets := make(map[uint32]uint64)
	if s.closing {
		return targets
	}
	for peer, viewer := range s.viewers {
		if viewer.request.Active && time.Since(viewer.seen) < 4*time.Second && screenPlaybackQuality(s.quality, viewer.request) == q {
			targets[peer] = viewer.request.RequestID
		}
	}
	return targets
}

func (s *screenShareSession) run() {
	defer close(s.done)
	outputs := make(map[media.VideoQuality]func())
	defer func() {
		for _, closeOutput := range outputs {
			closeOutput()
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-s.wake:
		case <-ticker.C:
		}
		desired := make(map[media.VideoQuality]struct{})
		s.mu.Lock()
		for peer, viewer := range s.viewers {
			if time.Since(viewer.seen) >= 4*time.Second {
				delete(s.viewers, peer)
				continue
			}
			if viewer.request.Active {
				q := screenPlaybackQuality(s.quality, viewer.request)
				if q != s.quality {
					desired[q] = struct{}{}
				}
			}
		}
		s.mu.Unlock()
		for q, closeOutput := range outputs {
			if _, ok := desired[q]; !ok {
				closeOutput()
				delete(outputs, q)
			}
		}
		for q := range desired {
			if _, ok := outputs[q]; ok {
				continue
			}
			profile := q
			var sequence uint32
			outputs[q] = s.base.AddOutput(q, func(frame media.VideoFrame) {
				sequence++
				s.send(frame, sequence, profile, false)
			})
			s.client.log("screen playback output: %dx%d@%d %dkbps", q.Width, q.Height, q.FPS, q.Bitrate)
		}
	}
}

func (s *screenShareSession) close() {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	close(s.stop)
	<-s.done
}

// The original broadcast remains available to legacy viewers; the updated
// relay filters it out for subscribers. A profile is encoded once, and sent
// only to the viewers requesting it, with their current request number.
func (s *screenShareSession) send(frame media.VideoFrame, sequence uint32, q media.VideoQuality, broadcast bool) {
	targets := s.targets(q)
	if broadcast && s.needsBroadcast() {
		targets[proto.BroadcastID] = 0
	}
	for peer, requestID := range targets {
		c := s.client
		c.mu.Lock()
		conn, key, cid := c.conn, c.dataKey, c.clientID
		c.mu.Unlock()
		if conn == nil {
			return
		}
		meta := proto.ScreenPayload{Kind: proto.ScreenVideo, HasAudio: s.hasAudio, StreamID: s.streamID,
			Sequence: sequence, TimestampNS: max(frame.TimestampNS-s.startPTS, 0), SubscriptionID: requestID}
		blob, err := crypto.Seal(key, proto.EncodeScreenPayload(meta, frame.Data))
		if err != nil {
			continue
		}
		if len(blob)+10 < proto.MaxFragmentPayload {
			packet := make([]byte, 10, 10+len(blob))
			packet[0], packet[9] = proto.TypeVideoData, frame.Type
			binary.BigEndian.PutUint32(packet[1:5], cid)
			binary.BigEndian.PutUint32(packet[5:9], peer)
			c.writeVideoPacket(conn, append(packet, blob...))
		} else {
			c.mu.Lock()
			c.frameSeq++
			fid := c.frameSeq
			c.mu.Unlock()
			for _, packet := range proto.EncodeVideoFragments(cid, peer, fid, frame.Type, blob) {
				c.writeVideoPacket(conn, packet)
			}
		}
	}
}

func (s *screenShareSession) needsBroadcast() bool {
	s.client.mu.Lock()
	var peers []uint32
	for id := range s.client.members {
		if id != s.client.clientID {
			peers = append(peers, id)
		}
	}
	s.client.mu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range peers {
		if !s.negotiated[id] {
			return true
		}
	}
	return false
}

func (c *Client) SetScreenPlayback(resolution byte, fps int) {
	if resolution > 4 || (fps != 0 && fps != 30 && fps != 60 && fps != 120) {
		return
	}
	c.mu.Lock()
	if c.screenPlaybackResolution == resolution && c.screenPlaybackFPS == byte(fps) {
		c.mu.Unlock()
		return
	}
	c.screenPlaybackResolution, c.screenPlaybackFPS = resolution, byte(fps)
	c.screenSubscriptionID++
	source := c.screenSource
	c.mu.Unlock()
	c.sendScreenSubscription(source, true)
}

func (c *Client) ScreenSubscriptionID() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.screenSubscriptionSources[c.screenSource] {
		return 0
	}
	return c.screenSubscriptionID
}

func (c *Client) SupportsScreenPlayback(source uint32) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.screenSubscriptionSources[source]
}

func (c *Client) sendScreenSubscription(source uint32, active bool) {
	c.mu.Lock()
	// A retry may have captured the previous source before a UI change.
	// Never combine that source with the new subscription number.
	if active && source != c.screenSource {
		c.mu.Unlock()
		return
	}
	conn, cid := c.conn, c.clientID
	request := proto.ScreenSubscription{RequestID: c.screenSubscriptionID, Active: active && source != 0 && source != cid,
		Resolution: c.screenPlaybackResolution, FPS: c.screenPlaybackFPS}
	if request.Active && !c.screenSubscriptionSources[source] {
		request.Active = false
		request.Legacy = true
	}
	if active && source == 0 {
		request.Legacy = true
	}
	c.mu.Unlock()
	if conn == nil || request.RequestID == 0 {
		return
	}
	if source == 0 {
		source = proto.BroadcastID
	}
	conn.Write(proto.EncodeMediaControl(cid, source, proto.MediaCtrlScreenSubscribe, proto.EncodeScreenSubscription(request)))
}

func (c *Client) screenControlLoop() {
	defer c.wg.Done()
	c.mu.Lock()
	stop := c.stop
	c.mu.Unlock()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
		}
		c.mu.Lock()
		source, conn, cid, sharing := c.screenSource, c.conn, c.clientID, c.screenShare != nil
		c.mu.Unlock()
		if conn == nil {
			return
		}
		c.sendScreenSubscription(source, true)
		if sharing {
			conn.Write(proto.EncodeMediaControl(cid, proto.BroadcastID, proto.MediaCtrlStartVideo, []byte{2}))
		}
	}
}
