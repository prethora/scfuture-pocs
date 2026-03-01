.PHONY: build deploy test clean

build:
	GOOS=linux GOARCH=amd64 go build -o bin/machine-agent ./cmd/machine-agent
	GOOS=linux GOARCH=amd64 go build -o bin/coordinator ./cmd/coordinator

deploy: build
	./scripts/layer-4.1/deploy.sh

test:
	./scripts/layer-4.1/test_suite.sh

clean:
	rm -rf bin/
