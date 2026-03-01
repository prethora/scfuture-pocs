.PHONY: build deploy test clean

build:
	GOOS=linux GOARCH=amd64 go build -o bin/machine-agent ./cmd/machine-agent

deploy: build
	./scripts/deploy.sh

test:
	./scripts/test_suite.sh

clean:
	rm -rf bin/
