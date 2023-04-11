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

## License

All the code within this repository is licensed under [Apache-2.0](/LICENSE).

```
Copyright 2023 Cockroach Labs, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
```
