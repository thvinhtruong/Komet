.PHONY: build test demo remote-demo release clean

build:
	go build -trimpath -ldflags "-s -w" -o bin/komet ./cmd/komet

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

release:
	for t in linux/amd64 linux/arm64 darwin/arm64 darwin/amd64 windows/amd64; do \
		os=$${t%/*}; arch=$${t#*/}; ext=$$([ $$os = windows ] && echo .exe); \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
			-o dist/komet-$$os-$$arch$$ext ./cmd/komet; \
	done

clean:
	rm -rf bin dist
