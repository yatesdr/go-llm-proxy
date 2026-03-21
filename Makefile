BINARY=go-llm-proxy

.PHONY: build linux linux-arm macos macos-arm windows clean all

build:
	go build -o $(BINARY) .

linux:
	GOOS=linux GOARCH=amd64 go build -o $(BINARY) .

linux-arm:
	GOOS=linux GOARCH=arm64 go build -o $(BINARY) .

macos:
	GOOS=darwin GOARCH=amd64 go build -o $(BINARY) .

macos-arm:
	GOOS=darwin GOARCH=arm64 go build -o $(BINARY) .

windows:
	GOOS=windows GOARCH=amd64 go build -o $(BINARY).exe .

all:
	mkdir -p dist
	GOOS=linux GOARCH=amd64 go build -o dist/$(BINARY)-linux-amd64 .
	GOOS=linux GOARCH=arm64 go build -o dist/$(BINARY)-linux-arm64 .
	GOOS=darwin GOARCH=amd64 go build -o dist/$(BINARY)-macos-amd64 .
	GOOS=darwin GOARCH=arm64 go build -o dist/$(BINARY)-macos-arm64 .
	GOOS=windows GOARCH=amd64 go build -o dist/$(BINARY)-windows-amd64.exe .

clean:
	rm -f $(BINARY) $(BINARY).exe
	rm -rf dist
