.PHONY: test test-integration build
test:
	go test -race ./...
test-integration:
	test -n "$$M0_TEST_DATABASE_URL"
	test -n "$$M0_TEST_DOCKER"
	go test -race -count=1 ./...
build:
	mkdir -p bin
	go build -o bin/control ./cmd/control
