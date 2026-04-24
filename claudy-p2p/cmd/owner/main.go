// Owner: joins a room as "owner", waits for viewer, accepts a DataChannel
// and echoes every message back in upper case. Keeps running until Ctrl+C.
package main

import (
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/claudy/p2p/internal/peer"
	"github.com/claudy/p2p/internal/signaling"
	"github.com/pion/webrtc/v4"
)

func main() {
	signalURL := flag.String("signal", "ws://127.0.0.1:7000/signal", "signaling URL")
	room := flag.String("room", "demo", "room id")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	sig, err := signaling.Dial(*signalURL, *room, "owner", "")
	if err != nil {
		log.Error("dial signal", "err", err)
		os.Exit(1)
	}
	defer sig.Close()
	log.Info("waiting for viewer", "room", *room)

	pc, err := peer.New(sig)
	if err != nil {
		log.Error("new peer", "err", err)
		os.Exit(1)
	}
	defer pc.Close()

	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Info("connection state", "state", s.String())
	})

	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		log.Info("data channel opened", "label", dc.Label())
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			text := string(msg.Data)
			log.Info("recv", "msg", text)
			reply := strings.ToUpper(text)
			if err := dc.SendText(reply); err != nil {
				log.Warn("send reply", "err", err)
			}
		})
	})

	// Owner is answerer: wait for viewer to tell us ready, then for offer.
	if _, err := sig.Ready(); err != nil {
		log.Error("ready", "err", err)
		os.Exit(1)
	}

	go signalLoop(pc, sig, log, true /*answerer*/)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Info("shutting down")
}

// signalLoop drains incoming envelopes. When the answerer receives the offer
// it creates and sends an answer. Initiator (viewer) handles answer symmetrically.
func signalLoop(pc *webrtc.PeerConnection, sig *signaling.Client, log *slog.Logger, answerer bool) {
	for {
		env, err := sig.Recv()
		if err != nil {
			log.Warn("signal recv", "err", err)
			return
		}
		consumed, err := peer.ApplyRemote(pc, env)
		if err != nil {
			log.Warn("apply remote", "err", err)
			continue
		}
		if consumed && env.Kind == "sdp" && answerer {
			ans, err := pc.CreateAnswer(nil)
			if err != nil {
				log.Error("create answer", "err", err)
				return
			}
			if err := pc.SetLocalDescription(ans); err != nil {
				log.Error("set local", "err", err)
				return
			}
			if err := sig.Send("sdp", pc.LocalDescription()); err != nil {
				log.Error("send answer", "err", err)
				return
			}
		}
	}
}
