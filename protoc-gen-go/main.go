package main

import (
	"io/ioutil"
	"os"

	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/protoc-gen-go/generator"
)

func main() {
	// 首先创建一个代码生成器generator，CodeGeneratorRequest、CodeGeneratorResponse
	// 结构体都被保存在generator中，CodeGenerateResponse中保存着代码生成过程中
	// 的错误状态信息，因此我们可以通过这个结构体提取错误状态并进行错误处理
	g := generator.New()

	// 从标准输入中读取CodeGeneratorRequest信息（标准输入已经被重定向到了父进程
	// protoc进程创建的管道stdout_pipe的读端，父进程会从管道的写端写入该请求信息）
	data, err := ioutil.ReadAll(os.Stdin)
	if err != nil {
		g.Error(err, "reading input")
	}

	// 读取到的数据时串行化之后的CodeGeneratorRequest，将其反串行化成CodeGeneratorRequest
	if err := proto.Unmarshal(data, g.Request); err != nil {
		g.Error(err, "parsing input proto")
	}

	// 检查CodeGeneratorRequest中待生成的源代码文件数量，数量为0则无需生成
	if len(g.Request.FileToGenerate) == 0 {
		g.Fail("no files to generate")
	}

	// 将CodeGeneratorRequest中传递给代码生成器的参数设置到protoc插件的代码生成器中
	g.CommandLineParameters(g.Request.GetParameter())

	// 前面的proto.Unmarshal(...)操作将stdin中的请求反串行化成了CodeGeneratorRequest，
	// 这里的g.WrapTypes()将请求中的一些descriptors进行进一步封装，方便后面引用
	g.WrapTypes()

	g.SetPackageNames()
	g.BuildTypeNameMap()

	// 生成所有的源代码文件
	g.GenerateAllFiles()

	// 将CodeGeneratorResponse对象进行串行化处理
	data, err = proto.Marshal(g.Response)
	if err != nil {
		g.Error(err, "failed to marshal output proto")
	}
	// 将串行化之后的CodeGenerateResponse对象数据写入标准输出（标准输出已经被
	// 重定向到了父进程protoc进程创建的管道stdin_pipe的写端，父进程从管道的读
	// 端读取这里的响应）
	_, err = os.Stdout.Write(data)
	if err != nil {
		g.Error(err, "failed to write output proto")
	}
}
