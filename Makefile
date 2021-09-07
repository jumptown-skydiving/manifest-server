.PHONY: all
all: manifest-server

.PHONY: manifest-server
manifest-server: protos
	go build -o manifest-server cmd/main.go

pkg/server/service.pb.go: pkg/server/service.proto
	protoc -I=. --go_out=. --go_opt=paths=source_relative pkg/server/service.proto

.PHONY: protos
protos: pkg/server/service.pb.go

.PHONY: clean
clean:
	go clean

.PHONY: distclean
distclean: clean
	rm pkg/server/*.pb.go
