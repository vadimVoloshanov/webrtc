package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/pion/webrtc/v3/examples/internal/signal"
	"github.com/pion/webrtc/v3/pkg/media"
	"github.com/pion/webrtc/v3/pkg/media/ivfreader"
	"github.com/pion/webrtc/v3/pkg/media/ivfwriter"
	"github.com/pion/webrtc/v3/pkg/media/oggreader"
	"github.com/pion/webrtc/v3/pkg/media/oggwriter"
)

const (
	audioInputFileName = "input.ogg"
	videoInputFileName = "input.ivf"
)

var (
	waitingСonnection bool = false
	connectionCount        = 0
)

var (
	sendingMessage string = "START"
	sendingCount   int    = 0
)

type peerConnectionState struct {
	peerConnection *webrtc.PeerConnection
	candidatesMux  sync.Mutex
	listenAddr     string
}

func getMessage() string {
	sendingCount++
	if sendingCount > connectionCount {
		sendingCount = 1
		sendingMessage = signal.RandSeq(15)
	}
	return sendingMessage
}

func saveToDisk(i media.Writer, track *webrtc.TrackRemote) {
	defer func() {
		if err := i.Close(); err != nil {
			panic(err)
		}
	}()

	for {
		rtpPacket, _, err := track.ReadRTP()
		if err != nil {
			panic(err)
		}
		if err := i.WriteRTP(rtpPacket); err != nil {
			panic(err)
		}
	}
}

func signalCandidate(addr string, c *webrtc.ICECandidate) error {
	payload := []byte(c.ToJSON().Candidate)
	resp, err := http.Post(fmt.Sprintf("http://%s/candidate", addr),
		"application/json; charset=utf-8", bytes.NewReader(payload))
	if err != nil {
		return err
	}

	if closeErr := resp.Body.Close(); closeErr != nil {
		return closeErr
	}

	return nil
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

func initOnFunctionsDataChannels(d *webrtc.DataChannel) {
	// Register channel opening handling
	d.OnOpen(func() {
		fmt.Printf("Data channel '%s'-'%d' open. Random messages will now be sent to any connected DataChannels every 5 seconds\n", d.Label(), d.ID())

		for range time.NewTicker(5 * time.Second).C {
			message := getMessage()
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
}

func createDataChannel(offerAddr string, peerConnection *webrtc.PeerConnection) {
	// Create a datachannel with label 'data'
	d, err := peerConnection.CreateDataChannel("data", nil)
	if err != nil {
		panic(err)
	}

	initOnFunctionsDataChannels(d)

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

		initOnFunctionsDataChannels(d)
	})
}

func forwardVideo(peerConnection *webrtc.PeerConnection, iceConnectedCtx context.Context) {
	// Create a video track
	videoTrack, videoTrackErr := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: "video/vp8"}, "video", "pion")
	if videoTrackErr != nil {
		panic(videoTrackErr)
	}

	rtpSender, videoTrackErr := peerConnection.AddTrack(videoTrack)
	if videoTrackErr != nil {
		panic(videoTrackErr)
	}

	// Read incoming RTCP packets
	// Before these packets are returned they are processed by interceptors. For things
	// like NACK this needs to be called.
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	go func() {
		// Open a IVF file and start reading using our IVFReader
		file, ivfErr := os.Open(videoInputFileName)
		if ivfErr != nil {
			panic(ivfErr)
		}

		ivf, header, ivfErr := ivfreader.NewWith(file)
		if ivfErr != nil {
			panic(ivfErr)
		}

		// Wait for connection established
		<-iceConnectedCtx.Done()

		// Send our video file frame at a time. Pace our sending so we send it at the same speed it should be played back as.
		// This isn't required since the video is timestamped, but we will such much higher loss if we send all at once.
		sleepTime := time.Millisecond * time.Duration((float32(header.TimebaseNumerator)/float32(header.TimebaseDenominator))*1000)
		for {
			frame, _, ivfErr := ivf.ParseNextFrame()
			if ivfErr == io.EOF {
				fmt.Println("All video frames parsed and sent")
				return
			}

			if ivfErr != nil {
				panic(ivfErr)
			}

			time.Sleep(sleepTime)
			if ivfErr = videoTrack.WriteSample(media.Sample{Data: frame, Duration: time.Second}); ivfErr != nil {
				panic(ivfErr)
			}
		}
	}()
}

func forwardAudio(peerConnection *webrtc.PeerConnection, iceConnectedCtx context.Context) {
	// Create a audio track
	audioTrack, audioTrackErr := webrtc.NewTrackLocalStaticSample(webrtc.RTPCodecCapability{MimeType: "audio/opus"}, "audio", "pion")
	if audioTrackErr != nil {
		panic(audioTrackErr)
	}

	rtpSenderAudio, audioTrackErr := peerConnection.AddTrack(audioTrack)
	if audioTrackErr != nil {
		panic(audioTrackErr)
	}

	// Read incoming RTCP packets
	// Before these packets are returned they are processed by interceptors. For things
	// like NACK this needs to be called.
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSenderAudio.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	go func() {
		// Open a IVF file and start reading using our IVFReader
		file, oggErr := os.Open(audioInputFileName)
		if oggErr != nil {
			panic(oggErr)
		}

		// Open on oggfile in non-checksum mode.
		ogg, _, oggErr := oggreader.NewWith(file)
		if oggErr != nil {
			panic(oggErr)
		}

		// Wait for connection established
		<-iceConnectedCtx.Done()

		// Keep track of last granule, the difference is the amount of samples in the buffer
		var lastGranule uint64
		for {
			pageData, pageHeader, oggErr := ogg.ParseNextPage()
			if oggErr == io.EOF {
				fmt.Println("All audio pages parsed and sent")
				return
			}

			if oggErr != nil {
				panic(oggErr)
			}

			// The amount of samples is the difference between the last and current timestamp
			sampleCount := float64(pageHeader.GranulePosition - lastGranule)
			lastGranule = pageHeader.GranulePosition
			sampleDuration := time.Duration((sampleCount/48000)*1000) * time.Millisecond

			if oggErr = audioTrack.WriteSample(media.Sample{Data: pageData, Duration: sampleDuration}); oggErr != nil {
				panic(oggErr)
			}

			time.Sleep(sampleDuration)
		}
	}()
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
	connectionCount++

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

	oggFile, err := oggwriter.New("output"+strconv.Itoa(len(*peerConnections))+".ogg", 48000, 2)
	if err != nil {
		panic(err)
	}
	h264File, err := ivfwriter.New("output" + strconv.Itoa(len(*peerConnections)) + ".ivf")
	if err != nil {
		panic(err)
	}

	iceConnectedCtx, iceConnectedCtxCancel := context.WithCancel(context.Background())

	// Set the handler for ICE connection state
	// This will notify you when the peer has connected/disconnected
	peerConnection.OnICEConnectionStateChange(func(connectionState webrtc.ICEConnectionState) {
		fmt.Printf("ICE Connection State has changed: %s\n", connectionState.String())
		iceConnectedCtxCancel()

		if connectionState == webrtc.ICEConnectionStateFailed || connectionState == webrtc.ICEConnectionStateDisconnected {
			closeErr := oggFile.Close()
			if closeErr != nil {
				panic(closeErr)
			}

			closeErr = h264File.Close()
			if closeErr != nil {
				panic(closeErr)
			}

			fmt.Println("Done writing media files")
		}
	})

	forwardVideo(peerConnection, iceConnectedCtx)
	forwardAudio(peerConnection, iceConnectedCtx)

	// Allow us to receive 1 audio track, and 1 video track
	if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio); err != nil {
		panic(err)
	} else if _, err = peerConnection.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo); err != nil {
		panic(err)
	}

	peerConnection.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		// Create a track to fan out our incoming video to all peers
		// trackLocal := addTrack(t)
		// defer removeTrack(trackLocal)

		// Send a PLI on an interval so that the publisher is pushing a keyframe every rtcpPLIInterval
		go func() {
			ticker := time.NewTicker(time.Second * 3)
			for range ticker.C {
				errSend := peerConnection.WriteRTCP([]rtcp.Packet{&rtcp.PictureLossIndication{MediaSSRC: uint32(t.SSRC())}})
				if errSend != nil {
					fmt.Println(errSend)
				}
			}
		}()
		codec := t.Codec()
		if strings.EqualFold(codec.MimeType, webrtc.MimeTypeOpus) {
			fmt.Println("Got Opus track, saving to disk as output.opus (48 kHz, 2 channels)")
			saveToDisk(oggFile, t)
		} else if strings.EqualFold(codec.MimeType, webrtc.MimeTypeVP8) {
			fmt.Println("Got VP8 track, saving to disk as output.ivf")
			saveToDisk(h264File, t)
		}
	})

	if waitingСonnection {
		waitDataChannel(peerConnection)
	} else {
		createDataChannel(offerAddr, peerConnection)
	}
}

func main() { //nolint:gocognit
	fmt.Println("Initialise drivers finish")
	offerAddr := flag.String("offer-address", "127.0.0.1:50000", "Address that the Offer HTTP server is hosted on.")
	answerAddr := flag.String("answer-address", "127.0.0.1:60000", "Address that the Answer HTTP server is hosted on.")
	flag.Parse()

	// Assert that we have an audio or video file
	_, err := os.Stat(videoInputFileName)
	haveVideoFile := !os.IsNotExist(err)

	_, err = os.Stat(audioInputFileName)
	haveAudioFile := !os.IsNotExist(err)

	if !haveAudioFile && !haveVideoFile {
		panic("Could not find `" + audioInputFileName + "` or `" + videoInputFileName + "`")
	}

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
		time.Sleep(3 * time.Second)
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
