package subscriber

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"sync"

	pb "github.com/SB-IM/pb/signal"
	"github.com/gorilla/mux"
	"github.com/pion/webrtc/v3"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"nhooyr.io/websocket"
	"nhooyr.io/websocket/wsjson"

	"github.com/SB-IM/skywalker/internal/broadcast/cfg"
	"github.com/SB-IM/skywalker/internal/broadcast/httpx"
	webrtcx "github.com/SB-IM/skywalker/internal/broadcast/webrtc"
)

// Subscriber stands for a subscriber webRTC peer.
type Subscriber struct {
	config cfg.WebRTCConfigOptions
	logger zerolog.Logger

	// sessions must be created before used by publisher and is shared between publishers ans subscribers.
	// It's only read by subscriber.
	sessions *sync.Map
}

// incomingMessage is a generic WebSocket incoming message.
type incomingMessage struct {
	Event string          `json:"event"`
	ID    string          `json:"id"`
	Data  json.RawMessage `json:"data"`
}

// outgoingMessage is a generic WebSocket outgoing message.
type outgoingMessage struct {
	Event string      `json:"event"`
	ID    string      `json:"id"`
	Data  interface{} `json:"data"`
}

// New returns a new Subscriber.
func New(sessions *sync.Map, logger *zerolog.Logger, config cfg.WebRTCConfigOptions) *Subscriber {
	l := logger.With().Str("component", "Subscriber").Logger()
	return &Subscriber{
		sessions: sessions,
		config:   config,
		logger:   l,
	}
}

// Signal performs webRTC signaling for all subscriber peers.
func (s *Subscriber) Signal() http.Handler {
	r := mux.NewRouter()
	r.HandleFunc("/v1/broadcast/signal", s.handleSignal()) // WebRTC SDP signaling. candidates trickling
	s.logger.Debug().Msg("registered signal HTTP handler")

	if s.config.EnableFrontend {
		r.Handle("/", http.FileServer(http.Dir("e2e/broadcast/static"))) // E2e static file server for debuging
		s.logger.Debug().Msg("registered broadcast e2e static file server handler")
	}
	return r
}

// handleSignal handles subscriber with webSocket api.
// Has candidate trickle support.
func (s *Subscriber) handleSignal() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: []string{"InsecureSkipVerify"},
		})
		if err != nil {
			s.logger.Err(err).Msg("could not upgrade to webSocket connection")
			return
		}
		defer c.Close(websocket.StatusNormalClosure, "")

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()

		s.processMessage(ctx, c)
	}
}

func (s *Subscriber) processMessage(ctx context.Context, c *websocket.Conn) {
	candidateChan := map[pb.TrackSource]chan string{
		pb.TrackSource_DRONE:   make(chan string, 2), // make buffer 2 because we send candidate at least twice.
		pb.TrackSource_MONITOR: make(chan string, 2), // make buffer 2 because we send candidate at least twice.
	}
	defer func() {
		for _, ch := range candidateChan {
			close(ch)
		}
	}()

	for {
		var msg incomingMessage
		if err := wsjson.Read(ctx, c, &msg); err != nil {
			s.logger.Err(err).Msg("could not read message")
			_ = replyErr(ctx, c, msg.ID, nil, httpx.ErrReadMessage)
			return
		}

		switch msg.Event {
		case "video-offer":
			var offer pb.SessionDescription
			if err := json.Unmarshal(msg.Data, &offer); err != nil {
				s.logger.Err(err).Msg("could not unmarshal JSON data")
				_ = replyErr(ctx, c, msg.ID, nil, httpx.ErrUnmarshalJSON)
				return
			}
			if offer.Meta == nil || offer.Meta.Id == "" {
				s.logger.Error().Msg("incorrect metadata")
				_ = replyErr(ctx, c, msg.ID, nil, httpx.ErrIncorrectMetadata)
				return
			}
			logger := s.logger.With().Str("event_id", msg.ID).Str("id", offer.Meta.Id).Int32("track_source", int32(offer.Meta.TrackSource)).Logger()
			logger.Debug().Msg("received offer from subscriber")

			sessionID := offer.Meta.Id + strconv.Itoa(int(offer.Meta.TrackSource))
			value, ok := s.sessions.Load(sessionID)
			if !ok {
				logger.Error().Msg("no machine id or track source found in existing sessions")
				_ = replyErr(ctx, c, msg.ID, offer.Meta, httpx.ErrMetadataNotMatched)
				return
			}

			wcx := webrtcx.New(s.config, &logger, sendCandidate(ctx, c, offer.Meta), recvCandidate(candidateChan[offer.Meta.TrackSource]))

			var sdp webrtc.SessionDescription
			if err := json.Unmarshal([]byte(offer.Sdp), &sdp); err != nil {
				s.logger.Err(err).Msg("could not unmarshal sdp")
				_ = replyErr(ctx, c, msg.ID, offer.Meta, httpx.ErrUnmarshalJSON)
				return
			}
			// TODO: handle blocking case with timeout for channels.
			wcx.SignalChan <- &sdp
			if err := wcx.CreateSubscriber(value.(*webrtc.TrackLocalStaticRTP)); err != nil {
				logger.Err(err).Msg("failed to create subscriber")
				_ = replyErr(ctx, c, msg.ID, offer.Meta, httpx.ErrFailedToCreateSubscriber)
				return
			}
			logger.Debug().Msg("successfully created subscriber")

			// TODO: Timeout channel receiving to avoid blocking.
			answer := <-wcx.SignalChan
			b, err := json.Marshal(answer)
			if err != nil {
				s.logger.Err(err).Msg("could not unmarshal answer to JSON")
				_ = replyErr(ctx, c, msg.ID, offer.Meta, httpx.ErrUnmarshalJSON)
				return
			}
			if err := wsjson.Write(ctx, c, &outgoingMessage{
				Event: "video-answer",
				Data: &pb.SessionDescription{
					Meta: offer.Meta,
					Sdp:  string(b),
				},
			}); err != nil {
				s.logger.Err(err).Msg("could not write answer JSON")
				return
			}
			logger.Debug().Msg("sent answer to subscriber")
		case "new-ice-candidate":
			var candidate pb.ICECandidate
			if err := json.Unmarshal(msg.Data, &candidate); err != nil {
				s.logger.Err(err).Msg("could not unmarshal JSON data")
				_ = replyErr(ctx, c, msg.ID, nil, httpx.ErrUnmarshalJSON)
				return
			}
			if candidate.Meta == nil || candidate.Meta.Id == "" {
				s.logger.Error().Msg("incorrect metadata")
				_ = replyErr(ctx, c, msg.ID, nil, httpx.ErrIncorrectMetadata)
				return
			}
			sessionID := candidate.Meta.Id + strconv.Itoa(int(candidate.Meta.TrackSource))
			_, ok := s.sessions.Load(sessionID)
			if !ok {
				s.logger.Error().Msg("no machine id or track source found in existing sessions")
				_ = replyErr(ctx, c, msg.ID, candidate.Meta, httpx.ErrMetadataNotMatched)
				return
			}

			var candidateInit webrtc.ICECandidateInit
			if err := json.Unmarshal([]byte(candidate.Candidate), &candidateInit); err != nil {
				s.logger.Err(err).Msg("could not unmarshal JSON candidate")
				_ = replyErr(ctx, c, msg.ID, candidate.Meta, httpx.ErrUnmarshalJSON)
				return
			}
			if candidate.Meta.TrackSource == pb.TrackSource_DRONE {
				candidateChan[pb.TrackSource_DRONE] <- candidateInit.Candidate
			} else {
				candidateChan[pb.TrackSource_MONITOR] <- candidateInit.Candidate
			}
		default:
			s.logger.Warn().Str("event", msg.Event).Msg("unknown event")
		}

		select {
		case <-ctx.Done():
			log.Debug().Msgf("context is done: %v", ctx.Err())
			return
		}
	}
}

// sendCandidate sends an ice candidate through webSocket.
// It can be called multiple time to send multiple ice candidates.
func sendCandidate(ctx context.Context, c *websocket.Conn, meta *pb.Meta) webrtcx.SendCandidateFunc {
	return func(candidate *webrtc.ICECandidate) error {
		// See: https://github.com/pion/example-webrtc-applications/blob/166d375aa9f8725e968758747e0d5bcf66d5b8dc/sfu-ws/main.go#L269-L269
		candidateJSON, err := json.Marshal(candidate.ToJSON())
		if err != nil {
			return err
		}
		return wsjson.Write(ctx, c, outgoingMessage{
			Event: "new-ice-candidate",
			Data: &pb.ICECandidate{
				Meta:      meta,
				Candidate: string(candidateJSON),
			},
		})
	}
}

// recvCandidate sends an ice candidate through webSocket.
// It continually reads from established webSocket connection getting ice candidates.
func recvCandidate(candidateChan <-chan string) webrtcx.RecvCandidateFunc {
	return func() <-chan string {
		return candidateChan
	}
}

// replyErr is an uniform error event reply to WebSocket client.
func replyErr(ctx context.Context, c *websocket.Conn, id string, meta *pb.Meta, code httpx.Code) error {
	type data struct {
		Meta *pb.Meta   `json:"meta,omitempty"`
		Code httpx.Code `json:"code"`
		Msg  string     `json:"message"`
	}
	return wsjson.Write(ctx, c, outgoingMessage{
		Event: "error",
		ID:    id,
		Data: data{
			Meta: meta,
			Code: code,
			Msg:  httpx.Errors[code],
		},
	})
}
