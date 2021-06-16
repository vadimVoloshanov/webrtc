# full-mesh
full-mesh is an example of several pion instances communicating directly!

The SDP offer and answer are exchanged automatically over HTTP.
The `answer` side acts like a HTTP server and should therefore be ran first.

## Instructions
First run `answer`:
```sh
export GO111MODULE=on
go install github.com/pion/webrtc/v3/examples/full-mesh/answer
answer
```
Next, run `offer`:
```sh
go install github.com/pion/webrtc/v3/examples/full-mesh/offer
offer
```

### Parameters
You can configure the addresses and ports on which the nodes will be launched
```
for answer:
answer-address example: "127.0.0.1:60000"

for offer:
answer-address example: "127.0.0.1:60000"
offer-address example "127.0.0.1:50000"
```


You should see them connect and start to exchange messages.
