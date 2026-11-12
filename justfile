build:
    go build -trimpath -ldflags "-X github.com/mrqwer/cairn/internal/cli.Version=$(git describe --always --dirty)" -o bin/cairn ./cmd/cairn

test:
    go test ./...

lint:
    golangci-lint run

install:
    sudo install -m 0755 -o root -g root bin/cairn /usr/local/bin/cairn
