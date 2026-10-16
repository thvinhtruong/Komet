.PHONY: build test demo remote-demo build-linux build-macos build-windows release clean

GOFLAGS := -trimpath -ldflags "-s -w"

build:
	go build $(GOFLAGS) -o bin/komet ./cmd/komet

test:
	go vet ./...
	go test ./...

demo: build
	go build -o bin/fakeserver ./tools/fakeserver
	./scripts/demo.sh

remote-demo: build
	go build -o bin/fakeserver ./tools/fakeserver
	go build -o bin/fakerunpod ./tools/fakerunpod
	./scripts/test-sshd.sh
	./scripts/remote-demo.sh

build-linux:
	mkdir -p build/linux
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build $(GOFLAGS) -o build/linux/komet-amd64 ./cmd/komet
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build $(GOFLAGS) -o build/linux/komet-arm64 ./cmd/komet

build-macos:
	mkdir -p build/macos
	GOOS=darwin GOARCH=amd64 CGO_ENABLED=0 go build $(GOFLAGS) -o build/macos/komet-amd64 ./cmd/komet
	GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build $(GOFLAGS) -o build/macos/komet-arm64 ./cmd/komet

build-windows:
	mkdir -p build/windows
	GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build $(GOFLAGS) -o build/windows/komet-amd64.exe ./cmd/komet

release: build-linux build-macos build-windows

clean:
	rm -rf bin build
