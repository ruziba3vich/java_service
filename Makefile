.PHONY: proto

proto:
	./generate_compiler_protos.sh

java-proto-gen:
	./generate_java_protos.sh
