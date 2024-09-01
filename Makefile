image=paskalmaksim/parallel-image-build:dev

test:
	go mod tidy
	go test ./...
	go run github.com/golangci/golangci-lint/cmd/golangci-lint@latest run -v

build:
	go run ./cmd/main.go --debug --tag=$(image)

install:
	go build -o /usr/local/bin/parallel-image-build ./cmd/main.go

run:
	docker run --pull=always --rm --entrypoint sh -v `pwd`:/app -it $(image)