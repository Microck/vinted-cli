.PHONY: build test check fmt vet lint install docs docs-check

build:
	go build -o bin/vinted ./cmd/vinted

test:
	go test ./... -count=1

fmt:
	gofmt -w .

vet:
	go vet ./...

lint:
	go run honnef.co/go/tools/cmd/staticcheck@latest ./...

check: vet lint test
	test -z "$$(gofmt -l .)"

# docs regenerates the command reference from the cobra tree. docs-check is
# what CI runs: it regenerates and fails if anything moved, so the published
# flags cannot drift from the binary's.
docs:
	go run ./cmd/docsgen

docs-check: docs
	git diff --exit-code -- docs-site/content/docs/reference
	test -z "$$(git ls-files --others --exclude-standard -- docs-site/content/docs/reference)"

install:
	go install -ldflags "-X main.version=$$(git describe --tags --always --dirty)" ./cmd/vinted
