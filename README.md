GOOS="linux" CGO_ENABLED="0" GOARCH=amd64 go build -o bin/deploy -ldflags "-s -w" main.go