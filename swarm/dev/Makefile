.PHONY: build cluster test

default: build

build:
	go build -o bin/swarm    github.com/tnechain/go-tnechain/cmd/swarm
	go build -o bin/gtst     github.com/tnechain/go-tnechain/cmd/gtst
	go build -o bin/bootnode github.com/tnechain/go-tnechain/cmd/bootnode

cluster: build
	scripts/boot-cluster.sh

test:
	go test -v github.com/tnechain/go-tnechain/swarm/...
