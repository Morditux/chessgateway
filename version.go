package gateway

// Version is the release version reported by `chessgateway -version`.
// Release builds set it via:
//
//	go build -ldflags "-X github.com/Morditux/chessgateway.Version=1.2.3" ./cmd/chessgateway
//
// Unstamped development builds report "dev".
var Version = "dev"
