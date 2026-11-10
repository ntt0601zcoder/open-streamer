.PHONY: test lint tools fmt tidy vet check cover covercheck fixtures

test:
	go test -race -shuffle=on -count=1 -timeout=5m ./...

lint:
	golangci-lint run ./...

tools:
	go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.11.3

fmt:
	@if command -v gofumpt >/dev/null 2>&1; then gofumpt -l -w .; else gofmt -l -w .; fi

tidy:
	go mod tidy

vet:
	go vet ./...

check: tidy vet lint test

cover:
	go test -race -shuffle=on -count=1 -timeout=5m -coverprofile=coverage.out ./...

covercheck:
	go run github.com/vladopajic/go-test-coverage/v2@v2.22.0 -c .testcoverage.yml

fixtures:
	sh testdata/fixtures/gen.sh
