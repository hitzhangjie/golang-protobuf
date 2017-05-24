# Not stored here, but plugin.proto is in https://github.com/google/protobuf/
# at src/google/protobuf/compiler/plugin.proto
# Also we need to fix an import.
regenerate:
	@echo WARNING! THIS RULE IS PROBABLY NOT RIGHT FOR YOUR INSTALLATION
	protoc --go_out=Mgoogle/protobuf/descriptor.proto=github.com/golang/protobuf/protoc-gen-go/descriptor:../../../../.. \
		-I$(HOME)/src/protobuf/include $(HOME)/src/protobuf/include/google/protobuf/compiler/plugin.proto

restore:
	cp plugin.pb.golden plugin.pb.go

preserve:
	cp plugin.pb.go plugin.pb.golden
