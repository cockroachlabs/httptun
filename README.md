# httptun

Tunnel TCP over a WebSocket connection. We use this with Google's
Identity-Aware Proxy to get an authenticated TCP connection.


To use it, start the server somewhere and then use `NewClient()`
in your client code. Use `(*Client).Dial()` to get a TCP connection
in the form of a `net.Conn`.

```sh
# Listen for Websocket connections on, ie. all network interfaces.
export HTTPTUN_LISTEN_ADDR=:80
# Proxy the tunneled TCP connections to some upstream.
export HTTPTUN_DST_ADDR=1.2.3.4:5678
# Start the server.
go run ./cmd/server
# There's a basic healthcheck at /health
curl localhost:80/health
#=> ok
# Establish a connection at /ws with a client constructed with NewClient().
```
