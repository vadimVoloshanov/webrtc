package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/examples/internal/signal"
)

var (
	waitingСonnection bool = false
)

type peerConnectionState struct {
	peerConnection *webrtc.PeerConnection
	candidatesMux  sync.Mutex
	listenAddr     string
}

func handshake(addr string, listen_addr string) ([]string, error) {
	json_data, err := json.Marshal(listen_addr)

	if err != nil {
		log.Fatal(err)
	}

	resp, err := http.Post(fmt.Sprintf("http://%s/handshake", addr), "application/json",
		bytes.NewBuffer(json_data))

	if err != nil {
		log.Fatal(err)
	}

	var res []string

	json.NewDecoder(resp.Body).Decode(&res)

	if closeErr := resp.Body.Close(); closeErr != nil {
		return res, closeErr
	}

	return res, nil
}

func signalCandidate(addr string, c *webrtc.ICECandidate) error {
	payload := []byte(c.ToJSON().Candidate)
	resp, err := http.Post(fmt.Sprintf("http://%s/candidate", addr), "application/json; charset=utf-8", bytes.NewReader(payload)) //nolint:noctx
	if err != nil {
		return err
	}

	if closeErr := resp.Body.Close(); closeErr != nil {
		return closeErr
	}

	return nil
}

func createDataChannel(offerAddr string, peerConnection *webrtc.PeerConnection) {
	// Create a datachannel with label 'data'
	dataChannel, err := peerConnection.CreateDataChannel("data", nil)
	if err != nil {
		panic(err)
	}

	// Register channel opening handling
	dataChannel.OnOpen(func() {
		fmt.Printf("Data channel '%s'-'%d' open. Random messages will now be sent to any connected DataChannels every 5 seconds\n", dataChannel.Label(), dataChannel.ID())

		for range time.NewTicker(5 * time.Second).C {
			message := signal.RandSeq(15)
			fmt.Printf("Sending '%s'\n", message)

			// Send the message as text
			sendTextErr := dataChannel.SendText(message)
			if sendTextErr != nil {
				panic(sendTextErr)
			}
		}
	})

	// Register text message handling
	dataChannel.OnMessage(func(msg webrtc.DataChannelMessage) {
		fmt.Printf("Message from DataChannel '%s': '%s'\n", dataChannel.Label(), string(msg.Data))
	})

	// Create an offer to send to the other process
	offer, err := peerConnection.CreateOffer(nil)
	if err != nil {
		panic(err)
	}

	// Sets the LocalDescription, and starts our UDP listeners
	// Note: this will start the gathering of ICE candidates
	if err = peerConnection.SetLocalDescription(offer); err != nil {
		panic(err)
	}

	// Send our offer to the HTTP server listening in the other process
	payload, err := json.Marshal(offer)
	if err != nil {
		panic(err)
	}
	resp, err := http.Post(fmt.Sprintf("http://%s/sdp", offerAddr), "application/json; charset=utf-8", bytes.NewReader(payload)) // nolint:noctx
	if err != nil {
		panic(err)
	} else if err := resp.Body.Close(); err != nil {
		panic(err)
	}
}

func waitDataChannel(peerConnection *webrtc.PeerConnection) {
	// Register data channel creation handling
	peerConnection.OnDataChannel(func(d *webrtc.DataChannel) {
		fmt.Printf("New DataChannel %s %d\n", d.Label(), d.ID())

		// Register channel opening handling
		d.OnOpen(func() {
			fmt.Printf("Data channel '%s'-'%d' open. Random messages will now be sent to any connected DataChannels every 5 seconds\n", d.Label(), d.ID())

			for range time.NewTicker(5 * time.Second).C {
				message := signal.RandSeq(15)
				fmt.Printf("Sending '%s'\n", message)

				// Send the message as text
				sendTextErr := d.SendText(message)
				if sendTextErr != nil {
					panic(sendTextErr)
				}
			}
		})

		// Register text message handling
		d.OnMessage(func(msg webrtc.DataChannelMessage) {
			fmt.Printf("Message from DataChannel '%s': '%s'\n", d.Label(), string(msg.Data))
		})
	})
}

func handshakeHandler(offerAddr string, peerConnections *[]peerConnectionState) {
	var candidatesMux sync.Mutex
	pendingCandidates := make([]*webrtc.ICECandidate, 0)

	// Prepare the configuration
	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	// Create a new RTCPeerConnection
	peerConnection, err := webrtc.NewPeerConnection(config)
	if err != nil {
		panic(err)
	}

	*peerConnections = append(*peerConnections, peerConnectionState{peerConnection, candidatesMux, offerAddr})

	// When an ICE candidate is available send to the other Pion instance
	// the other Pion instance will add this candidate by calling AddICECandidate
	peerConnection.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}

		candidatesMux.Lock()
		defer candidatesMux.Unlock()

		desc := peerConnection.RemoteDescription()
		if desc == nil {
			pendingCandidates = append(pendingCandidates, c)
		} else if onICECandidateErr := signalCandidate(offerAddr, c); onICECandidateErr != nil {
			panic(onICECandidateErr)
		}
	})

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("ICE Connection State has changed: %s\n", connectionState.String())
	})

	if waitingСonnection {
		waitDataChannel(peerConnection)
	} else {
		createDataChannel(offerAddr, peerConnection)
	}
}

func main() { //nolint:gocognit
	offerAddr := flag.String("offer-address", "127.0.0.1:50000", "Address that the Offer HTTP server is hosted on.")
	answerAddr := flag.String("answer-address", "127.0.0.1:60000", "Address that the Answer HTTP server is hosted on.")
	flag.Parse()

	var peerConnections []peerConnectionState

	listened, handshakeErr := handshake(*answerAddr, *offerAddr)

	fmt.Println(listened)

	if handshakeErr != nil {
		panic(handshakeErr)
	}

	http.HandleFunc("/handshake", func(w http.ResponseWriter, r *http.Request) {
		var res string
		json.NewDecoder(r.Body).Decode(&res)

		waitingСonnection = true
		handshakeHandler(res, &peerConnections)
	})

	// A HTTP handler that allows the other Pion instance to send us ICE candidates
	// This allows us to add ICE candidates faster, we don't have to wait for STUN or TURN
	// candidates which may be slower
	http.HandleFunc("/candidate", func(w http.ResponseWriter, r *http.Request) {
		candidate, candidateErr := ioutil.ReadAll(r.Body)
		if candidateErr != nil {
			panic(candidateErr)
		}
		if candidateErr := peerConnections[len(peerConnections)-1].peerConnection.AddICECandidate(webrtc.ICECandidateInit{Candidate: string(candidate)}); candidateErr != nil {
			panic(candidateErr)
		}
	})

	// A HTTP handler that processes a SessionDescription given to us from the other Pion process
	http.HandleFunc("/sdp", func(w http.ResponseWriter, r *http.Request) {
		sdp := webrtc.SessionDescription{}
		if sdpErr := json.NewDecoder(r.Body).Decode(&sdp); sdpErr != nil {
			panic(sdpErr)
		}

		if sdpErr := peerConnections[len(peerConnections)-1].peerConnection.SetRemoteDescription(sdp); sdpErr != nil {
			panic(sdpErr)
		}

		if !waitingСonnection {
			peerConnections[len(peerConnections)-1].candidatesMux.Lock()
			defer peerConnections[len(peerConnections)-1].candidatesMux.Unlock()
		} else {
			// Create an answer to send to the other process
			answer, err := peerConnections[len(peerConnections)-1].peerConnection.CreateAnswer(nil)
			if err != nil {
				panic(err)
			}

			// Send our answer to the HTTP server listening in the other process
			payload, err := json.Marshal(answer)
			if err != nil {
				panic(err)
			}
			resp, err := http.Post(fmt.Sprintf("http://%s/sdp", peerConnections[len(peerConnections)-1].listenAddr), "application/json; charset=utf-8", bytes.NewReader(payload)) // nolint:noctx
			if err != nil {
				panic(err)
			} else if closeErr := resp.Body.Close(); closeErr != nil {
				panic(closeErr)
			}

			// Sets the LocalDescription, and starts our UDP listeners
			err = peerConnections[len(peerConnections)-1].peerConnection.SetLocalDescription(answer)
			if err != nil {
				panic(err)
			}
		}
	})
	// Start HTTP server that accepts requests from the answer process
	go func() { panic(http.ListenAndServe(*offerAddr, nil)) }()

	handshakeHandler(*answerAddr, &peerConnections)

	for i, l := range listened {
		time.Sleep(5 * time.Second)
		fmt.Println(i, l)

		listened, handshakeErr := handshake(l, *offerAddr)
		fmt.Println(listened)
		if handshakeErr != nil {
			panic(handshakeErr)
		}

		waitingСonnection = false
		handshakeHandler(l, &peerConnections)
	}

	// Block forever
	select {}
}
