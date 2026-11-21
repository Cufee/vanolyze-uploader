DIST_DIR ?= dist
VERSION ?= v0.0.0
SEQUENCE ?= 0

release_package := github.com/Cufee/vanolyze-uploader/internal/release
ldflags := -s -w -buildid= -X $(release_package).Version=$(VERSION) -X $(release_package).Sequence=$(SEQUENCE)

.PHONY: build windows linux check vet test

build: windows linux

windows:
	mkdir -p "$(DIST_DIR)"
	CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags '$(ldflags) -H=windowsgui' -o "$(DIST_DIR)/vanolyze-uploader-windows-amd64.exe" ./cmd/vanolyze-uploader

linux:
	mkdir -p "$(DIST_DIR)"
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -buildvcs=false -ldflags '$(ldflags)' -o "$(DIST_DIR)/vanolyze-uploader-linux-amd64" ./cmd/vanolyze-uploader

check: vet test

vet:
	go vet ./...

test:
	go test ./...
