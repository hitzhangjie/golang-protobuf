// Go support for Protocol Buffers - Google's data interchange format
//
// Copyright 2015 The Go Authors.  All rights reserved.
// https://github.com/golang/protobuf
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are
// met:
//
//     * Redistributions of source code must retain the above copyright
// notice, this list of conditions and the following disclaimer.
//     * Redistributions in binary form must reproduce the above
// copyright notice, this list of conditions and the following disclaimer
// in the documentation and/or other materials provided with the
// distribution.
//     * Neither the name of Google Inc. nor the names of its
// contributors may be used to endorse or promote products derived from
// this software without specific prior written permission.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
// "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
// LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
// A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
// OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
// LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
// DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
// THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
// OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

// Package cgiservice outputs cgiservice description in java code.
// It runs as a plugin for the Go protocol buffer compiler plugin.
// It is linked in to protoc-gen-cgi.
package cgiservice

import (
	"fmt"
	"path"
	"strconv"
	"strings"

	"generator"
	pb "github.com/golang/protobuf/protoc-gen-go/descriptor"
)

// generatedCodeVersion indicates a version of the generated code.
// It is incremented whenever an incompatibility between the generated code and
// the cgiservice package is introduced; the generated code references
// a constant, cgiservice.SupportPackageIsVersionN (where N is generatedCodeVersion).
const generatedCodeVersion = 4

// Paths for packages used by code generated in this file,
// relative to the import_prefix of the generator.Generator.
const (
	contextPkgPath    = "golang.org/x/net/context"
	cgiservicePkgPath = "google.golang.org/cgiservice"
)

func init() {
	generator.RegisterPlugin(new(cgiservice))
}

// cgiservice is an implementation of the Go protocol buffer compiler's
// plugin architecture.  It generates bindings for cgiservice support.
type cgiservice struct {
	gen *generator.Generator
}

// Name returns the name of this plugin, "cgiservice".
func (g *cgiservice) Name() string {
	return "cgiservice"
}

// The names for packages imported in the generated code.
// They may vary from the final path component of the import path
// if the name is used by other packages.
var (
	contextPkg    string
	cgiservicePkg string
)

// Init initializes the plugin.
func (g *cgiservice) Init(gen *generator.Generator) {
	g.gen = gen
	contextPkg = generator.RegisterUniquePackageName("context", nil)
	cgiservicePkg = generator.RegisterUniquePackageName("cgiservice", nil)
}

// Given a type name defined in a .proto, return its object.
// Also record that we're using it, to guarantee the associated import.
func (g *cgiservice) objectNamed(name string) generator.Object {
	g.gen.RecordTypeUse(name)
	return g.gen.ObjectNamed(name)
}

// Given a type name defined in a .proto, return its name as we will print it.
func (g *cgiservice) typeName(str string) string {
	return g.gen.TypeName(g.objectNamed(str))
}

// In indent
func (g *cgiservice) In() { g.gen.In() }

// Out un-indent
func (g *cgiservice) Out() { g.gen.Out() }

// P forwards to g.gen.P.
func (g *cgiservice) P(args ...interface{}) { g.gen.P(args...) }

// Generate generates code for the services in the given file.
func (g *cgiservice) Generate(file *generator.FileDescriptor) {
	if len(file.FileDescriptorProto.Service) == 0 {
		return
	}

	g.P("// This file is generated by protoc-gen-cgi, protoc invoked with --cgi_out=plugins=cgiservice:dir")
	g.P("// ")
	g.P("// If any errors found, please email to zhijiezhang@tencent.com to report. Thanks in advance!")
	g.P("// Of course, you can edit the file to meet your needs, but good tips should be shared, so we can")
	g.P("// make this plugin better and better to accelerate our partners' developing effiency.")

	g.P()
	g.P("/**")
	g.P(" * CgiService Util Class")
	g.P(" * ")
	g.P(" * @author ${whoami}")
	g.P(" * @see    ${proto}")
	g.P(" */")

	// package
	g.P("package com.tencent.jungle.now.web.", file.PackageName(), ";")
	g.P()

	// import
	g.P("import com.google.inject.Inject;")
	g.P()

	// import PBWrappingClass for .proto files
	java_pkg_name := file.PackageName()
	java_outer_classname := getJavaOuterClassname(file)
	g.P("import ", java_pkg_name, ".", java_outer_classname, ";")
	// import other common classes in ${jungle-cgi-project}
	g.P("import com.tencent.jungle.web.config.CGIContext;")
	g.P("import com.tencent.jungle.web.config.ResourceCGISpecManager;")
	g.P("import com.tencent.jungle.web.config.adapters.CGIServiceAdapter;")
	g.P("import com.tencent.jungle.web.config.beans.CGIExecutorService;")
	g.P("import com.tencent.jungle.web.config.executor.CGIExecutor;")
	g.P()
	g.P("import kilim.Pausable;")
	g.P("import org.slf4j.Logger;")
	g.P("import org.slf4j.LoggerFactory;")
	g.P()
	g.P("import java.util.HashMap;")
	g.P("import java.util.Map;")
	g.P()

	// wrapping class
	// - classname
	classPrefix := "Gen"
	classSuffix := "WrapClass"
	fullClassName := classPrefix + generator.CamelCase(file.PackageName()) + classSuffix
	// - class comments
	g.P("/**")
	g.P(" * ", fullClassName)
	g.P(" */")
	// - class definition
	g.P("@Singleton")
	g.P("class ", fullClassName, " {")
	g.In()
	// -- class members
	g.P()
	g.P("// class members")
	// --- logging
	g.P("final Logger log = LoggerFactory.getLogger(", fullClassName, ".class);")
	// --- CGIServiceWrapping class declarations
	for i, service := range file.FileDescriptorProto.Service {
		g.generateCGIServiceAdapter(file, service, i)
	}

	g.Out()
	g.P("}")
	g.P()
}

// GenerateImports generates the import declaration for this file.
func (g *cgiservice) GenerateImports(file *generator.FileDescriptor) {
	if len(file.FileDescriptorProto.Service) == 0 {
		return
	}
	g.P("import (")
	g.P(contextPkg, " ", strconv.Quote(path.Join(g.gen.ImportPrefix, contextPkgPath)))
	g.P(cgiservicePkg, " ", strconv.Quote(path.Join(g.gen.ImportPrefix, cgiservicePkgPath)))
	g.P(")")
	g.P()
}

// reservedClientName records whether a client name is reserved on the client side.
var reservedClientName = map[string]bool{
// TODO: do we need any in cgiservice?
}

func unexport(s string) string { return strings.ToLower(s[:1]) + s[1:] }

// get value of 'option java_outer_classname=?'
func getJavaOuterClassname(file *generator.FileDescriptor) string {
	options := file.GetOptions()
	if options == nil {
		return ""
	} else {
		java_outer_classname := options.GetJavaOuterClassname()
		return java_outer_classname
	}
}

// generateService generates all the code for the named service.
func (g *cgiservice) generateCGIServiceAdapter(file *generator.FileDescriptor, service *pb.ServiceDescriptorProto, index int) {
	path := fmt.Sprintf("6,%d", index) // 6 means service.

	origServName := service.GetName()
	fullServName := origServName
	if pkg := file.GetPackage(); pkg != "" {
		fullServName = pkg + "." + fullServName
	}
	servName := generator.CamelCase(origServName)

	java_outer_classname := getJavaOuterClassname(file)

	g.P()
	g.P("/**")
	g.P(" * CGIServiceWrapping class for ", servName, " service")
	g.P(" */ ")
	g.P("class ", servName, " {")
	g.P()
	g.In()
	// + CGIServiceAdapter for each service interface
	for i, method := range service.Method {
		g.gen.PrintComments(fmt.Sprintf("%s,2,%d", path, i))
		origMethName := method.GetName()
		g.P("static CGIServiceAdapter ", generator.CamelCase(origMethName), " = null;")
		g.P()
	}
	// - inject all declared CGIServiceAdapter members
	g.P()
	g.P("@Inject")
	g.P("public ", servName, "(ResourceCGISpecManager manager) {")
	g.In()
	g.P()
	for _, method := range service.Method {
		origMethName := method.GetName()
		methName := generator.CamelCase(origMethName)
		g.P(servName, ".", methName, " = manager.getServiceAdapter(\"", generator.UpperCase(servName), "_CMD_", generator.UpperCase(origMethName), "\");")
	}
	g.Out()
	g.P("}")

	// - service method
	g.P()
	for i, method := range service.Method {
		origMethName := method.GetName()
		inputType := method.GetInputType()
		inputType = inputType[strings.LastIndex(inputType, ".")+1:]
		outputType := method.GetOutputType()
		outputType = outputType[strings.LastIndex(outputType, ".")+1:]

		g.gen.PrintComments(fmt.Sprintf("%s,2,%d", path, i))
		g.P("//")
		g.P("//@param cgiContext cgiContext contains params info to build ", inputType, " instance")
		g.P("//@return           return the ", outputType, " instance")
		g.P("public ", outputType, " Do", generator.CamelCase(origMethName), "(CGIContext cgiContext) {")

		g.P()
		g.In()

		g.P(java_outer_classname, ".", outputType, " result = null;")
		g.P("try {")
		g.In()
		// - build the pb request & update cgiContext
		g.P(java_outer_classname, ".", inputType, " pbReqBuilder = ", java_outer_classname, ".", inputType, ".newBuilder();")
		// -- resolve inputType & update pbReqBuilder
		//unsafe_file * generator.FileDescriptorProto = Unsafe.Pointer(file)
		for _, message := range file.MessageType {
			if typeName := message.GetName(); typeName == inputType {
				fields := message.GetField()
				for _, f := range fields {
					fname := f.GetName()
					ftype := f.GetTypeName()
					fvalue := f.GetDefaultValue()
					// providing ftype is primitive datatypes
					g.P("name=", fname, ", type=", ftype, ", value=", fvalue)

				}

				break
			}
		}

		g.P("cgiContext.setPbRequestMessage(pbReqBuilder.build());")
		// - call backend service
		g.P("result = ", "(", java_outer_classname, ".", outputType, ")", origMethName, ".doService(cgiContext);")
		g.Out()
		g.P("}")
		g.P("catch (Exception e) {")
		g.In()
		g.P("log.error(\"exception occurred, {}\", e);")
		g.Out()
		g.P("}")

		g.P()
		g.P("return result;")

		g.Out()
		g.P("}")
		g.P()
	}

	g.Out()
	g.P("}")

}
