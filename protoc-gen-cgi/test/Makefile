all: Test.proto
	protoc --cgi_out=plugins=cgiservice:. Test.proto
.PHONY: clean
clean:
	rm *.java *.xml
