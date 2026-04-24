// Viewer: joins a room as "viewer", opens a DataChannel, sends one message,
// prints the echo, and exits.
package main

import (
	"flag"
	"log/slog"
	"os"
	"time"

	"github.com/claudy/p2p/internal/peer"
	"github.com/claudy/p2p/internal/signaling"
	"github.com/pion/webrtc/v4"
)

func main() {
	signalURL := flag.String("signal", "ws://127.0.0.1:7000/signal", "signaling URL")
	room := flag.String("room", "demo", "room id")
	msg := flag.String("msg", "hello", "text to send over DataChannel")
	timeout := flag.Duration("timeout", 15*time.Second, "overall deadline")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	sig, err := signaling.Dial(*signalURL, *room, "viewer", "")
	if err != nil {
		log.Error("dial signal", "err", err)
		os.Exit(1)
	}
	defer sig.Close()

	if _, err := sig.Ready(); err != nil {
		log.Error("ready", "err", err)
		os.Exit(1)
	}
	log.Info("paired with owner", "room", *room)

	pc, err := peer.New(sig)
	if err != nil {
		log.Error("new peer", "err", err)
		os.Exit(1)
	}
	defer pc.Close()

	dc, err := pc.CreateDataChannel("claudy", nil)
	if err != nil {
		log.Error("create dc", "err", err)
		os.Exit(1)
	}

	replyCh := make(chan string, 1)
	dc.OnOpen(func() {
		log.Info("data channel open, sending", "msg", *msg)
		_ = dc.SendText(*msg)
	})
	dc.OnMessage(func(m webrtc.DataChannelMessage) {
		select {
		case replyCh <- string(m.Data):
		default:
		}
	})

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		log.Error("create offer", "err", err)
		os.Exit(1)
	}
	if err := pc.SetLocalDescription(offer); err != nil {
		log.Error("set local", "err", err)
		os.Exit(1)
	}
	if err := sig.Send("sdp", pc.LocalDescription()); err != nil {
		log.Error("send offer", "err", err)
		os.Exit(1)
	}

	go func() {
		for {
			env, err := sig.Recv()
			if err != nil {
				return
			}
			if _, err := peer.ApplyRemote(pc, env); err != nil {
				log.Warn("apply remote", "err", err)
			}
		}
	}()

	select {
	case reply := <-replyCh:
		log.Info("got echo", "reply", reply)
	case <-time.After(*timeout):
		log.Error("timed out waiting for echo")
		os.Exit(2)
	}
}
