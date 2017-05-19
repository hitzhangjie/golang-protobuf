package main

// import _ means import this package only for its side effects (initialization)
// import _ 之后，就完成了grpc子插件向protoc-gen-go这个插件的generator的注册，
// 也就是plugins []Plugin中将可以找到这个插件，之后通过--go_out=plugins=grpc:.
// 就可以在源代码生成过程中启用grpc插件。
import _ "github.com/golang/protobuf/protoc-gen-go/grpc"
