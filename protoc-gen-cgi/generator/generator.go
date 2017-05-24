// protoc的go语言插件代码生成器，从.proto文件生成go源代码
package generator

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"fmt"
	"go/parser"
	"go/printer"
	"go/token"
	"log"
	"os"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/golang/protobuf/proto"
	"github.com/golang/protobuf/protoc-gen-go/descriptor"

	plugin "github.com/golang/protobuf/protoc-gen-go/plugin"
)

// 生成源代码的版本
const generatedCodeVersion = 2

// go源代码生成过程中能够生成某些功能的go源代码的插件，例如生成rpc相关的代码
type Plugin interface {
	// 获取插件名字
	Name() string

	// 初始化一个插件，该方法在插件对象创建之后、代码生成之前被调用
	Init(g *Generator)

	// 通过调用代码生成器的P、In、Out方法生成与proto文件相关的源代码，生成的源
	// 代码中不包括imports语句
	Generate(file *FileDescriptor)

	// 生成import声明，虽然源代码文件中import语句出现在前面，但是实际上最后被
	// 扩展出来，实现方式就是通过插入点。GenerateImports方法在Generate方法调用
	// 之后再调用
	GenerateImports(file *FileDescriptor)
}

// protoc中可能启用了多个插件，所以这里用插件数组，注册的插件会被保存到plugins中
var plugins []Plugin

// 注册插件p即将其追加到plugins，注册插件操作一般在插件初始化过程中完成
func RegisterPlugin(p Plugin) {
	plugins = append(plugins, p)
}

// 导入的每一个proto文件都需要一个表示它的FileDescriptorProto指针来表示，这些
// FileDescriptorProto需要一个wrapping类型。首先顶一个common结构体，其中包括了
// 各个proto的FileDesciptorProto指针file，还增加了PackageName()、File()接口。
//
// 然后再用common这个结构体来定义FileDescriptor，和FileDescriptorProto相比删除
// 了最后一个单词Proto。
type common struct {
	file *descriptor.FileDescriptorProto // File this object comes from.
}

// 返回待生成的源代码文件中的package从句的包名
func (c *common) PackageName() string { return uniquePackageOf(c.file) }

// 返回待生成的源代码文件对应的FileDescriptorProto对象指针
func (c *common) File() *descriptor.FileDescriptorProto { return c.file }

// 检测proto文件是proto3版本
func fileIsProto3(file *descriptor.FileDescriptorProto) bool {
	return file.GetSyntax() == "proto3"
}
func (c *common) proto3() bool { return fileIsProto3(c.file) }

// 描述一个pb message
type Descriptor struct {
	common   *descriptor.DescriptorProto // 对应的DescriptorProto指针
	parent   *Descriptor                 // 上级message指针slice, 如果有的话
	nested   []*Descriptor               // 内部message指针slice，如果有的话
	enums    []*EnumDescriptor           // 内部enum指针slice, 如果有的话.
	ext      []*ExtensionDescriptor      // 扩展指针slice，如果有的话
	typename []string                    // 缓存的typename slice
	index    int                         // 在容器中的索引值，不管是file还是message
	path     string                      // SourceCodeInfo path，逗号分隔的数字
	group    bool
}

// 返回message的类型名称
// - 这里的message类型有可能是非顶层message，因此其名称可能包含了“.”；
// - 类型名称不包含package从句中定义的包名；
func (d *Descriptor) TypeName() []string {
	if d.typename != nil {
		return d.typename
	}
	n := 0
	for parent := d; parent != nil; parent = parent.parent {
		n++
	}
	s := make([]string, n, n)
	for parent := d; parent != nil; parent = parent.parent {
		n--
		s[n] = parent.GetName()
	}
	d.typename = s
	return s
}

// 描述一个枚举类型
// - 如果是顶层enum类型，其parent为nil；
// - 如果不是顶层enum类型，其parent为上层message类型；
type EnumDescriptor struct {
	common
	*descriptor.EnumDescriptorProto
	parent   *Descriptor // The containing message, if any.
	typename []string    // Cached typename vector.
	index    int         // The index into the container, whether the file or a message.
	path     string      // The SourceCodeInfo path as comma-separated integers.
}

// 返回enum的类型名称
// - 这里的enum类型有可能是非顶层enum，因此其名称可能包含了“.”；
// - 类型名称不包含package从句中定义的包名；
func (e *EnumDescriptor) TypeName() (s []string) {
	if e.typename != nil {
		return e.typename
	}
	name := e.GetName()
	if e.parent == nil {
		s = make([]string, 1)
	} else {
		pname := e.parent.TypeName()
		s = make([]string, len(pname)+1)
		copy(s, pname)
	}
	s[len(s)-1] = name
	e.typename = s
	return s
}

// 返回枚举类型名字的前缀
func (e *EnumDescriptor) prefix() string {
	if e.parent == nil {
		// If the enum is not part of a message, the prefix is just the type name.
		return CamelCase(*e.Name) + "_"
	}
	typeName := e.TypeName()
	return CamelCaseSlice(typeName[0:len(typeName)-1]) + "_"
}

// 返回枚举类型中的指定名字的枚举常量的整数值
func (e *EnumDescriptor) integerValueAsString(name string) string {
	for _, c := range e.Value {
		if c.GetName() == name {
			return fmt.Sprint(c.GetNumber())
		}
	}
	log.Fatal("cannot find value for enum constant")
	return ""
}

// 描述一个extension
// - 如果该扩展为顶层类型定义，parent不存在，parent值为nil；
// - 如果该扩展为message内部定义，parent为上层的message；
type ExtensionDescriptor struct {
	common
	*descriptor.FieldDescriptorProto
	parent *Descriptor // The containing message, if any.
}

// 返回extension的类型名
// - 这里的extension类型有可能是非顶层extension，因此其名称可能包含了“.”；
// - 类型名称不包含package从句中定义的包名；
func (e *ExtensionDescriptor) TypeName() (s []string) {
	name := e.GetName()
	if e.parent == nil {
		// top-level extension
		s = make([]string, 1)
	} else {
		pname := e.parent.TypeName()
		s = make([]string, len(pname)+1)
		copy(s, pname)
	}
	s[len(s)-1] = name
	return s
}

// 返回描述信息
func (e *ExtensionDescriptor) DescName() string {
	// The full type name.
	typeName := e.TypeName()
	// Each scope of the extension is individually CamelCased, and all are joined with "_" with an "E_" prefix.
	for i, s := range typeName {
		typeName[i] = CamelCase(s)
	}
	return "E_" + strings.Join(typeName, "_")
}

// 描述从另一个proto文件中publicly imported的类型
type ImportedDescriptor struct {
	common
	o Object
}

// 返回导入类型的类型名
func (id *ImportedDescriptor) TypeName() []string { return id.o.TypeName() }

// FileDescriptor描述一个.proto文件，通过这个类型来访问.proto文件的类型定义比
// FileDescriptorProto更方便。
// - 包括了所有message类型、enum类型构成的slices;
// - 这里的slices是由generator.WrapTypes()构建的，从第一个字段构建出其他4个字段；
type FileDescriptor struct {
	*descriptor.FileDescriptorProto
	desc []*Descriptor          // 当前.proto文件中定义的所有message类型
	enum []*EnumDescriptor      // 当前.proto文件中定义的所有enum类型
	ext  []*ExtensionDescriptor // 当前.proto文件中定义的所有顶层extensions
	imp  []*ImportedDescriptor  // 当前.proto文件中publicly import进来的所有类型

	// .proto文件中的注释以及源代码Location信息map
	comments map[string]*descriptor.SourceCodeInfo_Location

	// .proto文件中导出的所有对象和对应的符号信息map，用于支持public imports
	exported map[Object][]symbol

	// .proto文件在protoc命令行中proto文件列表中的索引值
	index int

	// 是否生成该.proto文件的proto3版本的源代码
	proto3 bool
}

// .proto文件要生成的源代码文件的package从句指定的包名
func (d *FileDescriptor) PackageName() string { return uniquePackageOf(d.FileDescriptorProto) }

// VarName is the variable name we'll use in the generated code to refer
// to the compressed bytes of this descriptor. It is not exported, so
// it is only valid inside the generated package.
func (d *FileDescriptor) VarName() string { return fmt.Sprintf("fileDescriptor%d", d.index) }

// goPackageOption interprets the file's go_package option.
// If there is no go_package, it returns ("", "", false).
// If there's a simple name, it returns ("", pkg, true).
// If the option implies an import path, it returns (impPath, pkg, true).
func (d *FileDescriptor) goPackageOption() (impPath, pkg string, ok bool) {
	pkg = d.GetOptions().GetGoPackage()
	if pkg == "" {
		return
	}
	ok = true
	// The presence of a slash implies there's an import path.
	slash := strings.LastIndex(pkg, "/")
	if slash < 0 {
		return
	}
	impPath, pkg = pkg, pkg[slash+1:]
	// A semicolon-delimited suffix overrides the package name.
	sc := strings.IndexByte(impPath, ';')
	if sc < 0 {
		return
	}
	impPath, pkg = impPath[:sc], impPath[sc+1:]
	return
}

// .proto文件要生成的源代码文件的package从句指定的包名
// - 如果.proto文件中指定了选项option go_package = "??"，那么package从句将使用这
//   里指定的包名，返回的explicit=true；
// - 如果.proto文件中未指定选项option go_package = "??"，那么将使用.proto第一
//   行的package包名，返回的explicit=false；
func (d *FileDescriptor) goPackageName() (name string, explicit bool) {
	// Does the file have a "go_package" option?
	if _, pkg, ok := d.goPackageOption(); ok {
		return pkg, true
	}

	// Does the file have a package clause?
	if pkg := d.GetPackage(); pkg != "" {
		return pkg, false
	}
	// Use the file base name.
	return baseName(d.GetName()), false
}

// goFileName returns the output name for the generated Go file.
func (d *FileDescriptor) goFileName() string {
	name := *d.Name
	if ext := path.Ext(name); ext == ".proto" || ext == ".protodevel" {
		name = name[:len(name)-len(ext)]
	}
	name += ".pb.go"

	// Does the file have a "go_package" option?
	// If it does, it may override the filename.
	if impPath, _, ok := d.goPackageOption(); ok && impPath != "" {
		// Replace the existing dirname with the declared import path.
		_, name = path.Split(name)
		name = path.Join(impPath, name)
		return name
	}

	return name
}

// 添加要导出的对象和符号映射
func (d *FileDescriptor) addExport(obj Object, sym symbol) {
	d.exported[obj] = append(d.exported[obj], sym)
}

// 描述一个导出的Go symbol
type symbol interface {
	// GenerateAlias should generate an appropriate alias
	// for the symbol from the named package.
	GenerateAlias(g *Generator, pkg string)
}

type messageSymbol struct {
	sym                         string
	hasExtensions, isMessageSet bool
	hasOneof                    bool
	getters                     []getterSymbol
}

type getterSymbol struct {
	name     string
	typ      string
	typeName string // canonical name in proto world; empty for proto.Message and similar
	genType  bool   // whether typ contains a generated type (message/group/enum)
}

func (ms *messageSymbol) GenerateAlias(g *Generator, pkg string) {
	remoteSym := pkg + "." + ms.sym

	g.P("type ", ms.sym, " ", remoteSym)
	g.P("func (m *", ms.sym, ") Reset() { (*", remoteSym, ")(m).Reset() }")
	g.P("func (m *", ms.sym, ") String() string { return (*", remoteSym, ")(m).String() }")
	g.P("func (*", ms.sym, ") ProtoMessage() {}")
	if ms.hasExtensions {
		g.P("func (*", ms.sym, ") ExtensionRangeArray() []", g.Pkg["proto"], ".ExtensionRange ",
			"{ return (*", remoteSym, ")(nil).ExtensionRangeArray() }")
		if ms.isMessageSet {
			g.P("func (m *", ms.sym, ") Marshal() ([]byte, error) ",
				"{ return (*", remoteSym, ")(m).Marshal() }")
			g.P("func (m *", ms.sym, ") Unmarshal(buf []byte) error ",
				"{ return (*", remoteSym, ")(m).Unmarshal(buf) }")
		}
	}
	if ms.hasOneof {
		// Oneofs and public imports do not mix well.
		// We can make them work okay for the binary format,
		// but they're going to break weirdly for text/JSON.
		enc := "_" + ms.sym + "_OneofMarshaler"
		dec := "_" + ms.sym + "_OneofUnmarshaler"
		size := "_" + ms.sym + "_OneofSizer"
		encSig := "(msg " + g.Pkg["proto"] + ".Message, b *" + g.Pkg["proto"] + ".Buffer) error"
		decSig := "(msg " + g.Pkg["proto"] + ".Message, tag, wire int, b *" + g.Pkg["proto"] + ".Buffer) (bool, error)"
		sizeSig := "(msg " + g.Pkg["proto"] + ".Message) int"
		g.P("func (m *", ms.sym, ") XXX_OneofFuncs() (func", encSig, ", func", decSig, ", func", sizeSig, ", []interface{}) {")
		g.P("return ", enc, ", ", dec, ", ", size, ", nil")
		g.P("}")

		g.P("func ", enc, encSig, " {")
		g.P("m := msg.(*", ms.sym, ")")
		g.P("m0 := (*", remoteSym, ")(m)")
		g.P("enc, _, _, _ := m0.XXX_OneofFuncs()")
		g.P("return enc(m0, b)")
		g.P("}")

		g.P("func ", dec, decSig, " {")
		g.P("m := msg.(*", ms.sym, ")")
		g.P("m0 := (*", remoteSym, ")(m)")
		g.P("_, dec, _, _ := m0.XXX_OneofFuncs()")
		g.P("return dec(m0, tag, wire, b)")
		g.P("}")

		g.P("func ", size, sizeSig, " {")
		g.P("m := msg.(*", ms.sym, ")")
		g.P("m0 := (*", remoteSym, ")(m)")
		g.P("_, _, size, _ := m0.XXX_OneofFuncs()")
		g.P("return size(m0)")
		g.P("}")
	}
	for _, get := range ms.getters {

		if get.typeName != "" {
			g.RecordTypeUse(get.typeName)
		}
		typ := get.typ
		val := "(*" + remoteSym + ")(m)." + get.name + "()"
		if get.genType {
			// typ will be "*pkg.T" (message/group) or "pkg.T" (enum)
			// or "map[t]*pkg.T" (map to message/enum).
			// The first two of those might have a "[]" prefix if it is repeated.
			// Drop any package qualifier since we have hoisted the type into this package.
			rep := strings.HasPrefix(typ, "[]")
			if rep {
				typ = typ[2:]
			}
			isMap := strings.HasPrefix(typ, "map[")
			star := typ[0] == '*'
			if !isMap { // map types handled lower down
				typ = typ[strings.Index(typ, ".")+1:]
			}
			if star {
				typ = "*" + typ
			}
			if rep {
				// Go does not permit conversion between slice types where both
				// element types are named. That means we need to generate a bit
				// of code in this situation.
				// typ is the element type.
				// val is the expression to get the slice from the imported type.

				ctyp := typ // conversion type expression; "Foo" or "(*Foo)"
				if star {
					ctyp = "(" + typ + ")"
				}

				g.P("func (m *", ms.sym, ") ", get.name, "() []", typ, " {")
				g.In()
				g.P("o := ", val)
				g.P("if o == nil {")
				g.In()
				g.P("return nil")
				g.Out()
				g.P("}")
				g.P("s := make([]", typ, ", len(o))")
				g.P("for i, x := range o {")
				g.In()
				g.P("s[i] = ", ctyp, "(x)")
				g.Out()
				g.P("}")
				g.P("return s")
				g.Out()
				g.P("}")
				continue
			}
			if isMap {
				// Split map[keyTyp]valTyp.
				bra, ket := strings.Index(typ, "["), strings.Index(typ, "]")
				keyTyp, valTyp := typ[bra+1:ket], typ[ket+1:]
				// Drop any package qualifier.
				// Only the value type may be foreign.
				star := valTyp[0] == '*'
				valTyp = valTyp[strings.Index(valTyp, ".")+1:]
				if star {
					valTyp = "*" + valTyp
				}

				typ := "map[" + keyTyp + "]" + valTyp
				g.P("func (m *", ms.sym, ") ", get.name, "() ", typ, " {")
				g.P("o := ", val)
				g.P("if o == nil { return nil }")
				g.P("s := make(", typ, ", len(o))")
				g.P("for k, v := range o {")
				g.P("s[k] = (", valTyp, ")(v)")
				g.P("}")
				g.P("return s")
				g.P("}")
				continue
			}
			// Convert imported type into the forwarding type.
			val = "(" + typ + ")(" + val + ")"
		}

		g.P("func (m *", ms.sym, ") ", get.name, "() ", typ, " { return ", val, " }")
	}

}

type enumSymbol struct {
	name   string
	proto3 bool // Whether this came from a proto3 file.
}

func (es enumSymbol) GenerateAlias(g *Generator, pkg string) {
	s := es.name
	g.P("type ", s, " ", pkg, ".", s)
	g.P("var ", s, "_name = ", pkg, ".", s, "_name")
	g.P("var ", s, "_value = ", pkg, ".", s, "_value")
	g.P("func (x ", s, ") String() string { return (", pkg, ".", s, ")(x).String() }")
	if !es.proto3 {
		g.P("func (x ", s, ") Enum() *", s, "{ return (*", s, ")((", pkg, ".", s, ")(x).Enum()) }")
		g.P("func (x *", s, ") UnmarshalJSON(data []byte) error { return (*", pkg, ".", s, ")(x).UnmarshalJSON(data) }")
	}
}

type constOrVarSymbol struct {
	sym  string
	typ  string // either "const" or "var"
	cast string // if non-empty, a type cast is required (used for enums)
}

func (cs constOrVarSymbol) GenerateAlias(g *Generator, pkg string) {
	v := pkg + "." + cs.sym
	if cs.cast != "" {
		v = cs.cast + "(" + v + ")"
	}
	g.P(cs.typ, " ", cs.sym, " = ", v)
}

// Object接口是对enum、message、extension、imported objects能力的抽象，可用Object来引用这些类型
type Object interface {
	PackageName() string // The name we use in our output (a_b_c), possibly renamed for uniqueness.
	TypeName() []string
	File() *descriptor.FileDescriptorProto
}

// Each package name we generate must be unique. The package we're generating
// gets its own name but every other package must have a unique name that does
// not conflict in the code we generate.  These names are chosen globally (although
// they don't have to be, it simplifies things to do them globally).
func uniquePackageOf(fd *descriptor.FileDescriptorProto) string {
	s, ok := uniquePackageName[fd]
	if !ok {
		log.Fatal("internal error: no package name defined for " + fd.GetName())
	}
	return s
}

// Generator类型的方法能够输出源代码，这些输出的源代码信息存储在Response成员中
type Generator struct {
	*bytes.Buffer

	Request  *plugin.CodeGeneratorRequest  // The input.
	Response *plugin.CodeGeneratorResponse // The output.

	Param             map[string]string // Command-line parameters.
	PackageImportPath string            // Go import path of the package we're generating code for
	ImportPrefix      string            // String to prefix to imported package file names.
	ImportMap         map[string]string // Mapping from .proto file name to import path

	Pkg map[string]string // The names under which we import support packages

	packageName      string                     // What we're calling ourselves.
	allFiles         []*FileDescriptor          // All files in the tree
	allFilesByName   map[string]*FileDescriptor // All files by filename.
	genFiles         []*FileDescriptor          // Those files we will generate output for.
	file             *FileDescriptor            // The file we are compiling now.
	usedPackages     map[string]bool            // Names of packages used in current file.
	typeNameToObject map[string]Object          // Key is a fully-qualified name in input syntax.
	init             []string                   // Lines to emit in the init function.
	indent           string
	writeOutput      bool
}

// 创建一个新的代码生成器，并创建请求、响应对象
func New() *Generator {
	g := new(Generator)
	g.Buffer = new(bytes.Buffer)
	g.Request = new(plugin.CodeGeneratorRequest)
	g.Response = new(plugin.CodeGeneratorResponse)
	return g
}

// 报告问题，包括错误，然后退出程序
func (g *Generator) Error(err error, msgs ...string) {
	s := strings.Join(msgs, " ") + ":" + err.Error()
	log.Print("protoc-gen-go: error:", s)
	os.Exit(1)
}

// 报告问题，然后退出程序
func (g *Generator) Fail(msgs ...string) {
	s := strings.Join(msgs, " ")
	log.Print("protoc-gen-go: error:", s)
	os.Exit(1)
}

// 将都好分隔的key=value列表解析成<key,value> map
func (g *Generator) CommandLineParameters(parameter string) {
	g.Param = make(map[string]string)
	for _, p := range strings.Split(parameter, ",") {
		if i := strings.Index(p, "="); i < 0 {
			g.Param[p] = ""
		} else {
			g.Param[p[0:i]] = p[i+1:]
		}
	}

	g.ImportMap = make(map[string]string)
	pluginList := "none" // Default list of plugin names to enable (empty means all).
	for k, v := range g.Param {
		switch k {
		case "import_prefix":
			g.ImportPrefix = v
		case "import_path":
			g.PackageImportPath = v
		// --go_out=plugins=grpc:.，解析这里的参数plugins=grpc
		case "plugins":
			pluginList = v
		default:
			if len(k) > 0 && k[0] == 'M' {
				g.ImportMap[k[1:]] = v
			}
		}
	}
	// 更新激活的插件列表
	if pluginList != "" {
		// Amend the set of plugins.
		enabled := make(map[string]bool)
		for _, name := range strings.Split(pluginList, "+") {
			enabled[name] = true
		}
		var nplugins []Plugin
		for _, p := range plugins {
			if enabled[p.Name()] {
				nplugins = append(nplugins, p)
			}
		}
		plugins = nplugins
	}
}

// DefaultPackageName returns the package name printed for the object.
// If its file is in a different package, it returns the package name we're using for this file, plus ".".
// Otherwise it returns the empty string.
func (g *Generator) DefaultPackageName(obj Object) string {
	pkg := obj.PackageName()
	if pkg == g.packageName {
		return ""
	}
	return pkg + "."
}

// For each input file, the unique package name to use, underscored.
var uniquePackageName = make(map[*descriptor.FileDescriptorProto]string)

// Package names already registered.  Key is the name from the .proto file;
// value is the name that appears in the generated code.
var pkgNamesInUse = make(map[string]bool)

// Create and remember a guaranteed unique package name for this file descriptor.
// Pkg is the candidate name.  If f is nil, it's a builtin package like "proto" and
// has no file descriptor.
func RegisterUniquePackageName(pkg string, f *FileDescriptor) string {
	// Convert dots to underscores before finding a unique alias.
	pkg = strings.Map(badToUnderscore, pkg)

	for i, orig := 1, pkg; pkgNamesInUse[pkg]; i++ {
		// It's a duplicate; must rename.
		pkg = orig + strconv.Itoa(i)
	}
	// Install it.
	pkgNamesInUse[pkg] = true
	if f != nil {
		uniquePackageName[f.FileDescriptorProto] = pkg
	}
	return pkg
}

var isGoKeyword = map[string]bool{
	"break":       true,
	"case":        true,
	"chan":        true,
	"const":       true,
	"continue":    true,
	"default":     true,
	"else":        true,
	"defer":       true,
	"fallthrough": true,
	"for":         true,
	"func":        true,
	"go":          true,
	"goto":        true,
	"if":          true,
	"import":      true,
	"interface":   true,
	"map":         true,
	"package":     true,
	"range":       true,
	"return":      true,
	"select":      true,
	"struct":      true,
	"switch":      true,
	"type":        true,
	"var":         true,
}

// defaultGoPackage returns the package name to use,
// derived from the import path of the package we're building code for.
func (g *Generator) defaultGoPackage() string {
	p := g.PackageImportPath
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[i+1:]
	}
	if p == "" {
		return ""
	}

	p = strings.Map(badToUnderscore, p)
	// Identifier must not be keyword: insert _.
	if isGoKeyword[p] {
		p = "_" + p
	}
	// Identifier must not begin with digit: insert _.
	if r, _ := utf8.DecodeRuneInString(p); unicode.IsDigit(r) {
		p = "_" + p
	}
	return p
}

// SetPackageNames sets the package name for this run.
// The package name must agree across all files being generated.
// It also defines unique package names for all imported files.
func (g *Generator) SetPackageNames() {
	// Register the name for this package.  It will be the first name
	// registered so is guaranteed to be unmodified.
	pkg, explicit := g.genFiles[0].goPackageName()

	// Check all files for an explicit go_package option.
	for _, f := range g.genFiles {
		thisPkg, thisExplicit := f.goPackageName()
		if thisExplicit {
			if !explicit {
				// Let this file's go_package option serve for all input files.
				pkg, explicit = thisPkg, true
			} else if thisPkg != pkg {
				g.Fail("inconsistent package names:", thisPkg, pkg)
			}
		}
	}

	// If we don't have an explicit go_package option but we have an
	// import path, use that.
	if !explicit {
		p := g.defaultGoPackage()
		if p != "" {
			pkg, explicit = p, true
		}
	}

	// If there was no go_package and no import path to use,
	// double-check that all the inputs have the same implicit
	// Go package name.
	if !explicit {
		for _, f := range g.genFiles {
			thisPkg, _ := f.goPackageName()
			if thisPkg != pkg {
				g.Fail("inconsistent package names:", thisPkg, pkg)
			}
		}
	}

	g.packageName = RegisterUniquePackageName(pkg, g.genFiles[0])

	// Register the support package names. They might collide with the
	// name of a package we import.
	g.Pkg = map[string]string{
		"fmt":   RegisterUniquePackageName("fmt", nil),
		"math":  RegisterUniquePackageName("math", nil),
		"proto": RegisterUniquePackageName("proto", nil),
	}

AllFiles:
	for _, f := range g.allFiles {
		for _, genf := range g.genFiles {
			if f == genf {
				// In this package already.
				uniquePackageName[f.FileDescriptorProto] = g.packageName
				continue AllFiles
			}
		}
		// The file is a dependency, so we want to ignore its go_package option
		// because that is only relevant for its specific generated output.
		pkg := f.GetPackage()
		if pkg == "" {
			pkg = baseName(*f.Name)
		}
		RegisterUniquePackageName(pkg, f)
	}
}

// WrapTypes walks the incoming data, wrapping DescriptorProtos, EnumDescriptorProtos
// and FileDescriptorProtos into file-referenced objects within the Generator.
// It also creates the list of files to generate and so should be called before GenerateAllFiles.
func (g *Generator) WrapTypes() {
	g.allFiles = make([]*FileDescriptor, 0, len(g.Request.ProtoFile))
	g.allFilesByName = make(map[string]*FileDescriptor, len(g.allFiles))
	for _, f := range g.Request.ProtoFile {
		// We must wrap the descriptors before we wrap the enums
		descs := wrapDescriptors(f)
		g.buildNestedDescriptors(descs)
		enums := wrapEnumDescriptors(f, descs)
		g.buildNestedEnums(descs, enums)
		exts := wrapExtensions(f)
		fd := &FileDescriptor{
			FileDescriptorProto: f,
			desc:                descs,
			enum:                enums,
			ext:                 exts,
			exported:            make(map[Object][]symbol),
			proto3:              fileIsProto3(f),
		}
		extractComments(fd)
		g.allFiles = append(g.allFiles, fd)
		g.allFilesByName[f.GetName()] = fd
	}
	for _, fd := range g.allFiles {
		fd.imp = wrapImported(fd.FileDescriptorProto, g)
	}

	g.genFiles = make([]*FileDescriptor, 0, len(g.Request.FileToGenerate))
	for _, fileName := range g.Request.FileToGenerate {
		fd := g.allFilesByName[fileName]
		if fd == nil {
			g.Fail("could not find file named", fileName)
		}
		fd.index = len(g.genFiles)
		g.genFiles = append(g.genFiles, fd)
	}
}

// Scan the descriptors in this file.  For each one, build the slice of nested descriptors
func (g *Generator) buildNestedDescriptors(descs []*Descriptor) {
	for _, desc := range descs {
		if len(desc.NestedType) != 0 {
			for _, nest := range descs {
				if nest.parent == desc {
					desc.nested = append(desc.nested, nest)
				}
			}
			if len(desc.nested) != len(desc.NestedType) {
				g.Fail("internal error: nesting failure for", desc.GetName())
			}
		}
	}
}

func (g *Generator) buildNestedEnums(descs []*Descriptor, enums []*EnumDescriptor) {
	for _, desc := range descs {
		if len(desc.EnumType) != 0 {
			for _, enum := range enums {
				if enum.parent == desc {
					desc.enums = append(desc.enums, enum)
				}
			}
			if len(desc.enums) != len(desc.EnumType) {
				g.Fail("internal error: enum nesting failure for", desc.GetName())
			}
		}
	}
}

// Construct the Descriptor
func newDescriptor(desc *descriptor.DescriptorProto, parent *Descriptor, file *descriptor.FileDescriptorProto, index int) *Descriptor {
	d := &Descriptor{
		common:          common{file},
		DescriptorProto: desc,
		parent:          parent,
		index:           index,
	}
	if parent == nil {
		d.path = fmt.Sprintf("%d,%d", messagePath, index)
	} else {
		d.path = fmt.Sprintf("%s,%d,%d", parent.path, messageMessagePath, index)
	}

	// The only way to distinguish a group from a message is whether
	// the containing message has a TYPE_GROUP field that matches.
	if parent != nil {
		parts := d.TypeName()
		if file.Package != nil {
			parts = append([]string{*file.Package}, parts...)
		}
		exp := "." + strings.Join(parts, ".")
		for _, field := range parent.Field {
			if field.GetType() == descriptor.FieldDescriptorProto_TYPE_GROUP && field.GetTypeName() == exp {
				d.group = true
				break
			}
		}
	}

	for _, field := range desc.Extension {
		d.ext = append(d.ext, &ExtensionDescriptor{common{file}, field, d})
	}

	return d
}

// Return a slice of all the Descriptors defined within this file
func wrapDescriptors(file *descriptor.FileDescriptorProto) []*Descriptor {
	sl := make([]*Descriptor, 0, len(file.MessageType)+10)
	for i, desc := range file.MessageType {
		sl = wrapThisDescriptor(sl, desc, nil, file, i)
	}
	return sl
}

// Wrap this Descriptor, recursively
func wrapThisDescriptor(sl []*Descriptor, desc *descriptor.DescriptorProto, parent *Descriptor, file *descriptor.FileDescriptorProto, index int) []*Descriptor {
	sl = append(sl, newDescriptor(desc, parent, file, index))
	me := sl[len(sl)-1]
	for i, nested := range desc.NestedType {
		sl = wrapThisDescriptor(sl, nested, me, file, i)
	}
	return sl
}

// Construct the EnumDescriptor
func newEnumDescriptor(desc *descriptor.EnumDescriptorProto, parent *Descriptor, file *descriptor.FileDescriptorProto, index int) *EnumDescriptor {
	ed := &EnumDescriptor{
		common:              common{file},
		EnumDescriptorProto: desc,
		parent:              parent,
		index:               index,
	}
	if parent == nil {
		ed.path = fmt.Sprintf("%d,%d", enumPath, index)
	} else {
		ed.path = fmt.Sprintf("%s,%d,%d", parent.path, messageEnumPath, index)
	}
	return ed
}

// Return a slice of all the EnumDescriptors defined within this file
func wrapEnumDescriptors(file *descriptor.FileDescriptorProto, descs []*Descriptor) []*EnumDescriptor {
	sl := make([]*EnumDescriptor, 0, len(file.EnumType)+10)
	// Top-level enums.
	for i, enum := range file.EnumType {
		sl = append(sl, newEnumDescriptor(enum, nil, file, i))
	}
	// Enums within messages. Enums within embedded messages appear in the outer-most message.
	for _, nested := range descs {
		for i, enum := range nested.EnumType {
			sl = append(sl, newEnumDescriptor(enum, nested, file, i))
		}
	}
	return sl
}

// Return a slice of all the top-level ExtensionDescriptors defined within this file.
func wrapExtensions(file *descriptor.FileDescriptorProto) []*ExtensionDescriptor {
	var sl []*ExtensionDescriptor
	for _, field := range file.Extension {
		sl = append(sl, &ExtensionDescriptor{common{file}, field, nil})
	}
	return sl
}

// Return a slice of all the types that are publicly imported into this file.
func wrapImported(file *descriptor.FileDescriptorProto, g *Generator) (sl []*ImportedDescriptor) {
	for _, index := range file.PublicDependency {
		df := g.fileByName(file.Dependency[index])
		for _, d := range df.desc {
			if d.GetOptions().GetMapEntry() {
				continue
			}
			sl = append(sl, &ImportedDescriptor{common{file}, d})
		}
		for _, e := range df.enum {
			sl = append(sl, &ImportedDescriptor{common{file}, e})
		}
		for _, ext := range df.ext {
			sl = append(sl, &ImportedDescriptor{common{file}, ext})
		}
	}
	return
}

func extractComments(file *FileDescriptor) {
	file.comments = make(map[string]*descriptor.SourceCodeInfo_Location)
	for _, loc := range file.GetSourceCodeInfo().GetLocation() {
		if loc.LeadingComments == nil {
			continue
		}
		var p []string
		for _, n := range loc.Path {
			p = append(p, strconv.Itoa(int(n)))
		}
		file.comments[strings.Join(p, ",")] = loc
	}
}

// BuildTypeNameMap builds the map from fully qualified type names to objects.
// The key names for the map come from the input data, which puts a period at the beginning.
// It should be called after SetPackageNames and before GenerateAllFiles.
func (g *Generator) BuildTypeNameMap() {
	g.typeNameToObject = make(map[string]Object)
	for _, f := range g.allFiles {
		// The names in this loop are defined by the proto world, not us, so the
		// package name may be empty.  If so, the dotted package name of X will
		// be ".X"; otherwise it will be ".pkg.X".
		dottedPkg := "." + f.GetPackage()
		if dottedPkg != "." {
			dottedPkg += "."
		}
		for _, enum := range f.enum {
			name := dottedPkg + dottedSlice(enum.TypeName())
			g.typeNameToObject[name] = enum
		}
		for _, desc := range f.desc {
			name := dottedPkg + dottedSlice(desc.TypeName())
			g.typeNameToObject[name] = desc
		}
	}
}

// ObjectNamed, given a fully-qualified input type name as it appears in the input data,
// returns the descriptor for the message or enum with that name.
func (g *Generator) ObjectNamed(typeName string) Object {
	o, ok := g.typeNameToObject[typeName]
	if !ok {
		g.Fail("can't find object with type", typeName)
	}

	// If the file of this object isn't a direct dependency of the current file,
	// or in the current file, then this object has been publicly imported into
	// a dependency of the current file.
	// We should return the ImportedDescriptor object for it instead.
	direct := *o.File().Name == *g.file.Name
	if !direct {
		for _, dep := range g.file.Dependency {
			if *g.fileByName(dep).Name == *o.File().Name {
				direct = true
				break
			}
		}
	}
	if !direct {
		found := false
	Loop:
		for _, dep := range g.file.Dependency {
			df := g.fileByName(*g.fileByName(dep).Name)
			for _, td := range df.imp {
				if td.o == o {
					// Found it!
					o = td
					found = true
					break Loop
				}
			}
		}
		if !found {
			log.Printf("protoc-gen-go: WARNING: failed finding publicly imported dependency for %v, used in %v", typeName, *g.file.Name)
		}
	}

	return o
}

// P(...)将参数输出到生成的go源代码中
func (g *Generator) P(str ...interface{}) {
	if !g.writeOutput {
		return
	}
	g.WriteString(g.indent)
	for _, v := range str {
		switch s := v.(type) {
		case string:
			g.WriteString(s)
		case *string:
			g.WriteString(*s)
		case bool:
			fmt.Fprintf(g, "%t", s)
		case *bool:
			fmt.Fprintf(g, "%t", *s)
		case int:
			fmt.Fprintf(g, "%d", s)
		case *int32:
			fmt.Fprintf(g, "%d", *s)
		case *int64:
			fmt.Fprintf(g, "%d", *s)
		case float64:
			fmt.Fprintf(g, "%g", s)
		case *float64:
			fmt.Fprintf(g, "%g", *s)
		default:
			g.Fail(fmt.Sprintf("unknown type in printer: %T", v))
		}
	}
	g.WriteByte('\n')
}

// addInitf stores the given statement to be printed inside the file's init function.
// The statement is given as a format specifier and arguments.
func (g *Generator) addInitf(stmt string, a ...interface{}) {
	g.init = append(g.init, fmt.Sprintf(stmt, a...))
}

// 向生成的源代码中输出一个tabstop，实现indent功能
func (g *Generator) In() { g.indent += "\t" }

// 从生成的源代码中删除一个tabstop，实现unindent功能
func (g *Generator) Out() {
	if len(g.indent) > 0 {
		g.indent = g.indent[1:]
	}
}

// 生成所有.proto文件对应的go源代码，这里只是将源代码内容存储到g.Response中，
// 并没有直接创建源代码文件，插件将Response传递给protoc进程后由protoc进程来负
// 责创建源代码文件
func (g *Generator) GenerateAllFiles() {
	// Initialize the plugins
	for _, p := range plugins {
		p.Init(g)
	}
	// Generate the output. The generator runs for every file, even the files
	// that we don't generate output for, so that we can collate the full list
	// of exported symbols to support public imports.
	genFileMap := make(map[*FileDescriptor]bool, len(g.genFiles))
	for _, file := range g.genFiles {
		genFileMap[file] = true
	}
	for _, file := range g.allFiles {
		g.Reset()
		g.writeOutput = genFileMap[file]
		g.generate(file)
		if !g.writeOutput {
			continue
		}
		g.Response.File = append(g.Response.File, &plugin.CodeGeneratorResponse_File{
			Name:    proto.String(file.goFileName()),
			Content: proto.String(g.String()),
		})
	}
}

// Run all the plugins associated with the file.
func (g *Generator) runPlugins(file *FileDescriptor) {
	// 在上述generator处理的基础上，继续运行generator中注册的插件，依次运行插件
	for _, p := range plugins {
		p.Generate(file)
	}
}

// FileOf return the FileDescriptor for this FileDescriptorProto.
func (g *Generator) FileOf(fd *descriptor.FileDescriptorProto) *FileDescriptor {
	for _, file := range g.allFiles {
		if file.FileDescriptorProto == fd {
			return file
		}
	}
	g.Fail("could not find file in table:", fd.GetName())
	return nil
}

// 针对.proto文件（由FileDescriptor表示）生成对应的go源代码
func (g *Generator) generate(file *FileDescriptor) {
	g.file = g.FileOf(file.FileDescriptorProto)
	g.usedPackages = make(map[string]bool)

	if g.file.index == 0 {
		// For one file in the package, assert version compatibility.
		g.P("// This is a compile-time assertion to ensure that this generated file")
		g.P("// is compatible with the proto package it is being compiled against.")
		g.P("// A compilation error at this line likely means your copy of the")
		g.P("// proto package needs to be updated.")
		g.P("const _ = ", g.Pkg["proto"], ".ProtoPackageIsVersion", generatedCodeVersion, " // please upgrade the proto package")
		g.P()
	}

	// 生成import语句
	for _, td := range g.file.imp {
		g.generateImported(td)
	}
	// 生成enum类型定义
	for _, enum := range g.file.enum {
		g.generateEnum(enum)
	}
	// 生成message类型定义
	for _, desc := range g.file.desc {
		// Don't generate virtual messages for maps.
		if desc.GetOptions().GetMapEntry() {
			continue
		}
		g.generateMessage(desc)
	}
	// 生成extension类型定义
	for _, ext := range g.file.ext {
		g.generateExtension(ext)
	}
	// 生成初始化函数
	g.generateInitFunction()

	// 待输出的完整码需要知道哪些package是需要import的，哪些不需要，因此先运行
	// 插件生成go代码中除import之外的其他部分代码，然后知道了哪些package需要
	// import，再插入具体的import语句
	g.runPlugins(file)

	g.generateFileDescriptor(file)

	// 最后在go源代码中插入header、import
	rem := g.Buffer
	g.Buffer = new(bytes.Buffer)
	g.generateHeader()
	g.generateImports()
	if !g.writeOutput {
		return
	}
	g.Write(rem.Bytes())

	// 重新格式化生成的go源代码（gofmt）
	fset := token.NewFileSet()
	raw := g.Bytes()
	ast, err := parser.ParseFile(fset, "", g, parser.ParseComments)
	if err != nil {
		// Print out the bad code with line numbers.
		// This should never happen in practice, but it can while changing generated code,
		// so consider this a debugging aid.
		var src bytes.Buffer
		s := bufio.NewScanner(bytes.NewReader(raw))
		for line := 1; s.Scan(); line++ {
			fmt.Fprintf(&src, "%5d\t%s\n", line, s.Bytes())
		}
		g.Fail("bad Go source code was generated:", err.Error(), "\n"+src.String())
	}
	g.Reset()
	err = (&printer.Config{Mode: printer.TabIndent | printer.UseSpaces, Tabwidth: 8}).Fprint(g, fset, ast)
	if err != nil {
		g.Fail("generated Go source code could not be reformatted:", err.Error())
	}
}

// 生成header，包括package的定义
func (g *Generator) generateHeader() {
	g.P("// Code generated by protoc-gen-go. DO NOT EDIT.")
	g.P("// source: ", g.file.Name)
	g.P()

	name := g.file.PackageName()

	if g.file.index == 0 {
		// Generate package docs for the first file in the package.
		g.P("/*")
		g.P("Package ", name, " is a generated protocol buffer package.")
		g.P()
		if loc, ok := g.file.comments[strconv.Itoa(packagePath)]; ok {
			// not using g.PrintComments because this is a /* */ comment block.
			text := strings.TrimSuffix(loc.GetLeadingComments(), "\n")
			for _, line := range strings.Split(text, "\n") {
				line = strings.TrimPrefix(line, " ")
				// ensure we don't escape from the block comment
				line = strings.Replace(line, "*/", "* /", -1)
				g.P(line)
			}
			g.P()
		}
		var topMsgs []string
		g.P("It is generated from these files:")
		for _, f := range g.genFiles {
			g.P("\t", f.Name)
			for _, msg := range f.desc {
				if msg.parent != nil {
					continue
				}
				topMsgs = append(topMsgs, CamelCaseSlice(msg.TypeName()))
			}
		}
		g.P()
		g.P("It has these top-level messages:")
		for _, msg := range topMsgs {
			g.P("\t", msg)
		}
		g.P("*/")
	}

	g.P("package ", name)
	g.P()
}

// 打印.proto文件中对该location path关联的leading comments注释信息
func (g *Generator) PrintComments(path string) bool {
	if !g.writeOutput {
		return false
	}
	if loc, ok := g.file.comments[path]; ok {
		text := strings.TrimSuffix(loc.GetLeadingComments(), "\n")
		for _, line := range strings.Split(text, "\n") {
			g.P("// ", strings.TrimPrefix(line, " "))
		}
		return true
	}
	return false
}

func (g *Generator) fileByName(filename string) *FileDescriptor {
	return g.allFilesByName[filename]
}

// weak returns whether the ith import of the current file is a weak import.
func (g *Generator) weak(i int32) bool {
	for _, j := range g.file.WeakDependency {
		if j == i {
			return true
		}
	}
	return false
}

// Generate the imports
func (g *Generator) generateImports() {
	// We almost always need a proto import.  Rather than computing when we
	// do, which is tricky when there's a plugin, just import it and
	// reference it later. The same argument applies to the fmt and math packages.
	g.P("import " + g.Pkg["proto"] + " " + strconv.Quote(g.ImportPrefix+"github.com/golang/protobuf/proto"))
	g.P("import " + g.Pkg["fmt"] + ` "fmt"`)
	g.P("import " + g.Pkg["math"] + ` "math"`)
	for i, s := range g.file.Dependency {
		fd := g.fileByName(s)
		// Do not import our own package.
		if fd.PackageName() == g.packageName {
			continue
		}
		filename := fd.goFileName()
		// By default, import path is the dirname of the Go filename.
		importPath := path.Dir(filename)
		if substitution, ok := g.ImportMap[s]; ok {
			importPath = substitution
		}
		importPath = g.ImportPrefix + importPath
		// Skip weak imports.
		if g.weak(int32(i)) {
			g.P("// skipping weak import ", fd.PackageName(), " ", strconv.Quote(importPath))
			continue
		}
		// We need to import all the dependencies, even if we don't reference them,
		// because other code and tools depend on having the full transitive closure
		// of protocol buffer types in the binary.
		pname := fd.PackageName()
		if _, ok := g.usedPackages[pname]; !ok {
			pname = "_"
		}
		g.P("import ", pname, " ", strconv.Quote(importPath))
	}
	g.P()
	// TODO: may need to worry about uniqueness across plugins
	for _, p := range plugins {
		p.GenerateImports(g.file)
		g.P()
	}
	g.P("// Reference imports to suppress errors if they are not otherwise used.")
	g.P("var _ = ", g.Pkg["proto"], ".Marshal")
	g.P("var _ = ", g.Pkg["fmt"], ".Errorf")
	g.P("var _ = ", g.Pkg["math"], ".Inf")
	g.P()
}

func (g *Generator) generateImported(id *ImportedDescriptor) {
	// Don't generate public import symbols for files that we are generating
	// code for, since those symbols will already be in this package.
	// We can't simply avoid creating the ImportedDescriptor objects,
	// because g.genFiles isn't populated at that stage.
	tn := id.TypeName()
	sn := tn[len(tn)-1]
	df := g.FileOf(id.o.File())
	filename := *df.Name
	for _, fd := range g.genFiles {
		if *fd.Name == filename {
			g.P("// Ignoring public import of ", sn, " from ", filename)
			g.P()
			return
		}
	}
	g.P("// ", sn, " from public import ", filename)
	g.usedPackages[df.PackageName()] = true

	for _, sym := range df.exported[id.o] {
		sym.GenerateAlias(g, df.PackageName())
	}

	g.P()
}

// 生成指定enum类型的go源代码
func (g *Generator) generateEnum(enum *EnumDescriptor) {
	// The full type name
	typeName := enum.TypeName()
	// The full type name, CamelCased.
	ccTypeName := CamelCaseSlice(typeName)
	ccPrefix := enum.prefix()

	g.PrintComments(enum.path)
	g.P("type ", ccTypeName, " int32")
	g.file.addExport(enum, enumSymbol{ccTypeName, enum.proto3()})
	g.P("const (")
	g.In()
	for i, e := range enum.Value {
		g.PrintComments(fmt.Sprintf("%s,%d,%d", enum.path, enumValuePath, i))

		name := ccPrefix + *e.Name
		g.P(name, " ", ccTypeName, " = ", e.Number)
		g.file.addExport(enum, constOrVarSymbol{name, "const", ccTypeName})
	}
	g.Out()
	g.P(")")
	g.P("var ", ccTypeName, "_name = map[int32]string{")
	g.In()
	generated := make(map[int32]bool) // avoid duplicate values
	for _, e := range enum.Value {
		duplicate := ""
		if _, present := generated[*e.Number]; present {
			duplicate = "// Duplicate value: "
		}
		g.P(duplicate, e.Number, ": ", strconv.Quote(*e.Name), ",")
		generated[*e.Number] = true
	}
	g.Out()
	g.P("}")
	g.P("var ", ccTypeName, "_value = map[string]int32{")
	g.In()
	for _, e := range enum.Value {
		g.P(strconv.Quote(*e.Name), ": ", e.Number, ",")
	}
	g.Out()
	g.P("}")

	if !enum.proto3() {
		g.P("func (x ", ccTypeName, ") Enum() *", ccTypeName, " {")
		g.In()
		g.P("p := new(", ccTypeName, ")")
		g.P("*p = x")
		g.P("return p")
		g.Out()
		g.P("}")
	}

	g.P("func (x ", ccTypeName, ") String() string {")
	g.In()
	g.P("return ", g.Pkg["proto"], ".EnumName(", ccTypeName, "_name, int32(x))")
	g.Out()
	g.P("}")

	if !enum.proto3() {
		g.P("func (x *", ccTypeName, ") UnmarshalJSON(data []byte) error {")
		g.In()
		g.P("value, err := ", g.Pkg["proto"], ".UnmarshalJSONEnum(", ccTypeName, `_value, data, "`, ccTypeName, `")`)
		g.P("if err != nil {")
		g.In()
		g.P("return err")
		g.Out()
		g.P("}")
		g.P("*x = ", ccTypeName, "(value)")
		g.P("return nil")
		g.Out()
		g.P("}")
	}

	var indexes []string
	for m := enum.parent; m != nil; m = m.parent {
		// XXX: skip groups?
		indexes = append([]string{strconv.Itoa(m.index)}, indexes...)
	}
	indexes = append(indexes, strconv.Itoa(enum.index))
	g.P("func (", ccTypeName, ") EnumDescriptor() ([]byte, []int) { return ", g.file.VarName(), ", []int{", strings.Join(indexes, ", "), "} }")
	if enum.file.GetPackage() == "google.protobuf" && enum.GetName() == "NullValue" {
		g.P("func (", ccTypeName, `) XXX_WellKnownType() string { return "`, enum.GetName(), `" }`)
	}

	g.P()
}

// The tag is a string like "varint,2,opt,name=fieldname,def=7" that
// identifies details of the field for the protocol buffer marshaling and unmarshaling
// code.  The fields are:
//	wire encoding
//	protocol tag number
//	opt,req,rep for optional, required, or repeated
//	packed whether the encoding is "packed" (optional; repeated primitives only)
//	name= the original declared name
//	enum= the name of the enum type if it is an enum-typed field.
//	proto3 if this field is in a proto3 message
//	def= string representation of the default value, if any.
// The default value must be in a representation that can be used at run-time
// to generate the default value. Thus bools become 0 and 1, for instance.
func (g *Generator) goTag(message *Descriptor, field *descriptor.FieldDescriptorProto, wiretype string) string {
	optrepreq := ""
	switch {
	case isOptional(field):
		optrepreq = "opt"
	case isRequired(field):
		optrepreq = "req"
	case isRepeated(field):
		optrepreq = "rep"
	}
	var defaultValue string
	if dv := field.DefaultValue; dv != nil { // set means an explicit default
		defaultValue = *dv
		// Some types need tweaking.
		switch *field.Type {
		case descriptor.FieldDescriptorProto_TYPE_BOOL:
			if defaultValue == "true" {
				defaultValue = "1"
			} else {
				defaultValue = "0"
			}
		case descriptor.FieldDescriptorProto_TYPE_STRING,
			descriptor.FieldDescriptorProto_TYPE_BYTES:
			// Nothing to do. Quoting is done for the whole tag.
		case descriptor.FieldDescriptorProto_TYPE_ENUM:
			// For enums we need to provide the integer constant.
			obj := g.ObjectNamed(field.GetTypeName())
			if id, ok := obj.(*ImportedDescriptor); ok {
				// It is an enum that was publicly imported.
				// We need the underlying type.
				obj = id.o
			}
			enum, ok := obj.(*EnumDescriptor)
			if !ok {
				log.Printf("obj is a %T", obj)
				if id, ok := obj.(*ImportedDescriptor); ok {
					log.Printf("id.o is a %T", id.o)
				}
				g.Fail("unknown enum type", CamelCaseSlice(obj.TypeName()))
			}
			defaultValue = enum.integerValueAsString(defaultValue)
		}
		defaultValue = ",def=" + defaultValue
	}
	enum := ""
	if *field.Type == descriptor.FieldDescriptorProto_TYPE_ENUM {
		// We avoid using obj.PackageName(), because we want to use the
		// original (proto-world) package name.
		obj := g.ObjectNamed(field.GetTypeName())
		if id, ok := obj.(*ImportedDescriptor); ok {
			obj = id.o
		}
		enum = ",enum="
		if pkg := obj.File().GetPackage(); pkg != "" {
			enum += pkg + "."
		}
		enum += CamelCaseSlice(obj.TypeName())
	}
	packed := ""
	if (field.Options != nil && field.Options.GetPacked()) ||
		// Per https://developers.google.com/protocol-buffers/docs/proto3#simple:
		// "In proto3, repeated fields of scalar numeric types use packed encoding by default."
		(message.proto3() && (field.Options == nil || field.Options.Packed == nil) &&
			isRepeated(field) && isScalar(field)) {
		packed = ",packed"
	}
	fieldName := field.GetName()
	name := fieldName
	if *field.Type == descriptor.FieldDescriptorProto_TYPE_GROUP {
		// We must use the type name for groups instead of
		// the field name to preserve capitalization.
		// type_name in FieldDescriptorProto is fully-qualified,
		// but we only want the local part.
		name = *field.TypeName
		if i := strings.LastIndex(name, "."); i >= 0 {
			name = name[i+1:]
		}
	}
	if json := field.GetJsonName(); json != "" && json != name {
		// TODO: escaping might be needed, in which case
		// perhaps this should be in its own "json" tag.
		name += ",json=" + json
	}
	name = ",name=" + name
	if message.proto3() {
		// We only need the extra tag for []byte fields;
		// no need to add noise for the others.
		if *field.Type == descriptor.FieldDescriptorProto_TYPE_BYTES {
			name += ",proto3"
		}

	}
	oneof := ""
	if field.OneofIndex != nil {
		oneof = ",oneof"
	}
	return strconv.Quote(fmt.Sprintf("%s,%d,%s%s%s%s%s%s",
		wiretype,
		field.GetNumber(),
		optrepreq,
		packed,
		name,
		enum,
		oneof,
		defaultValue))
}

func needsStar(typ descriptor.FieldDescriptorProto_Type) bool {
	switch typ {
	case descriptor.FieldDescriptorProto_TYPE_GROUP:
		return false
	case descriptor.FieldDescriptorProto_TYPE_MESSAGE:
		return false
	case descriptor.FieldDescriptorProto_TYPE_BYTES:
		return false
	}
	return true
}

// TypeName is the printed name appropriate for an item. If the object is in the current file,
// TypeName drops the package name and underscores the rest.
// Otherwise the object is from another package; and the result is the underscored
// package name followed by the item name.
// The result always has an initial capital.
func (g *Generator) TypeName(obj Object) string {
	return g.DefaultPackageName(obj) + CamelCaseSlice(obj.TypeName())
}

// TypeNameWithPackage is like TypeName, but always includes the package
// name even if the object is in our own package.
func (g *Generator) TypeNameWithPackage(obj Object) string {
	return obj.PackageName() + CamelCaseSlice(obj.TypeName())
}

// GoType returns a string representing the type name, and the wire type
func (g *Generator) GoType(message *Descriptor, field *descriptor.FieldDescriptorProto) (typ string, wire string) {
	// TODO: Options.
	switch *field.Type {
	case descriptor.FieldDescriptorProto_TYPE_DOUBLE:
		typ, wire = "float64", "fixed64"
	case descriptor.FieldDescriptorProto_TYPE_FLOAT:
		typ, wire = "float32", "fixed32"
	case descriptor.FieldDescriptorProto_TYPE_INT64:
		typ, wire = "int64", "varint"
	case descriptor.FieldDescriptorProto_TYPE_UINT64:
		typ, wire = "uint64", "varint"
	case descriptor.FieldDescriptorProto_TYPE_INT32:
		typ, wire = "int32", "varint"
	case descriptor.FieldDescriptorProto_TYPE_UINT32:
		typ, wire = "uint32", "varint"
	case descriptor.FieldDescriptorProto_TYPE_FIXED64:
		typ, wire = "uint64", "fixed64"
	case descriptor.FieldDescriptorProto_TYPE_FIXED32:
		typ, wire = "uint32", "fixed32"
	case descriptor.FieldDescriptorProto_TYPE_BOOL:
		typ, wire = "bool", "varint"
	case descriptor.FieldDescriptorProto_TYPE_STRING:
		typ, wire = "string", "bytes"
	case descriptor.FieldDescriptorProto_TYPE_GROUP:
		desc := g.ObjectNamed(field.GetTypeName())
		typ, wire = "*"+g.TypeName(desc), "group"
	case descriptor.FieldDescriptorProto_TYPE_MESSAGE:
		desc := g.ObjectNamed(field.GetTypeName())
		typ, wire = "*"+g.TypeName(desc), "bytes"
	case descriptor.FieldDescriptorProto_TYPE_BYTES:
		typ, wire = "[]byte", "bytes"
	case descriptor.FieldDescriptorProto_TYPE_ENUM:
		desc := g.ObjectNamed(field.GetTypeName())
		typ, wire = g.TypeName(desc), "varint"
	case descriptor.FieldDescriptorProto_TYPE_SFIXED32:
		typ, wire = "int32", "fixed32"
	case descriptor.FieldDescriptorProto_TYPE_SFIXED64:
		typ, wire = "int64", "fixed64"
	case descriptor.FieldDescriptorProto_TYPE_SINT32:
		typ, wire = "int32", "zigzag32"
	case descriptor.FieldDescriptorProto_TYPE_SINT64:
		typ, wire = "int64", "zigzag64"
	default:
		g.Fail("unknown type for", field.GetName())
	}
	if isRepeated(field) {
		typ = "[]" + typ
	} else if message != nil && message.proto3() {
		return
	} else if field.OneofIndex != nil && message != nil {
		return
	} else if needsStar(*field.Type) {
		typ = "*" + typ
	}
	return
}

func (g *Generator) RecordTypeUse(t string) {
	if obj, ok := g.typeNameToObject[t]; ok {
		// Call ObjectNamed to get the true object to record the use.
		obj = g.ObjectNamed(t)
		g.usedPackages[obj.PackageName()] = true
	}
}

// Method names that may be generated.  Fields with these names get an
// underscore appended. Any change to this set is a potential incompatible
// API change because it changes generated field names.
var methodNames = [...]string{
	"Reset",
	"String",
	"ProtoMessage",
	"Marshal",
	"Unmarshal",
	"ExtensionRangeArray",
	"ExtensionMap",
	"Descriptor",
}

// Names of messages in the `google.protobuf` package for which
// we will generate XXX_WellKnownType methods.
var wellKnownTypes = map[string]bool{
	"Any":       true,
	"Duration":  true,
	"Empty":     true,
	"Struct":    true,
	"Timestamp": true,

	"Value":       true,
	"ListValue":   true,
	"DoubleValue": true,
	"FloatValue":  true,
	"Int64Value":  true,
	"UInt64Value": true,
	"Int32Value":  true,
	"UInt32Value": true,
	"BoolValue":   true,
	"StringValue": true,
	"BytesValue":  true,
}

// 生成参数message *Descriptor所代表的类型、默认常量定义相关的go源代码
func (g *Generator) generateMessage(message *Descriptor) {
	// 获取message类型名称
	typeName := message.TypeName()         // The full type name
	ccTypeName := CamelCaseSlice(typeName) // The full type name, CamelCased.

	// 获取message类型中要加入的方法名称
	usedNames := make(map[string]bool)
	for _, n := range methodNames {
		usedNames[n] = true
	}

	// message中定义的字段的名称、getter方法名称、类型名称等记录在这几个map里面
	// - k=FieldDescriptorProto，v=提取出的字段名称
	fieldNames := make(map[*descriptor.FieldDescriptorProto]string)
	// - k=FieldDescriptorProto，v=根据字段名称拼接处的getter方法名称
	fieldGetterNames := make(map[*descriptor.FieldDescriptorProto]string)
	// - k=FieldDescriptorProto，v=提取出的字段类型名称
	fieldTypes := make(map[*descriptor.FieldDescriptorProto]string)
	// - k=FieldDescriptorProto，v=对应的字段类型（fixme）
	mapFieldTypes := make(map[*descriptor.FieldDescriptorProto]string)

	// - k=字段的tag编号，v=字段名称
	oneofFieldName := make(map[int32]string)
	// - k=字段的tag编号，v=字段的getter方法名字
	oneofDisc := make(map[int32]string)
	// - k=字段的tag编号，v=字段的类型名称
	oneofTypeName := make(map[*descriptor.FieldDescriptorProto]string)
	// - k=字段的tag编号，v=插入点偏移量，g.Buffer中的偏移量 fixme???
	oneofInsertPoints := make(map[int32]int) // oneof_index => offset of g.Buffer

	// 生成message类型上方的注释信息（leading comments)
	g.PrintComments(message.path)
	// 生成定义message类型的go代码“type 类型名 struct ...”
	// - 后面就是遍历解析后的各个字段定义，然后针对各个字段生成响应的go代码；
	// - 生成go代码的时候，需要注意代码的缩进，因此会结合使用In()\Out()来控制缩进；
	g.P("type ", ccTypeName, " struct {")
	g.In()

	// 为ns...string中出现的字符串分配一个变量名，分配的时候要保证ns[i]这个变
	// 量名之前没有使用过，如果使用过的话，需要修改ns[i]中的变量名，采取的方式
	// 是加一个下划线作为后缀，再次检查之前有没有使用过，直到该分配的变量名没
	// 有被使用为止
	allocNames := func(ns ...string) []string {
	Loop:
		for {
			for _, n := range ns {
				if usedNames[n] {
					for i := range ns {
						ns[i] += "_"
					}
					continue Loop
				}
			}
			for _, n := range ns {
				usedNames[n] = true
			}
			return ns
		}
	}

	// 遍历message中定义的各个字段，并生成每个字段相关的go源代码
	// - 字段本身、字段的getter方法
	for i, field := range message.Field {
		// 获取定义的字段名称
		base := CamelCase(*field.Name)
		// 为字段名base及其getter方法分配无冲突的名称
		// - 字段定义顺序调整后分配的名称可能会发生改变，将allocNames方法注释）
		ns := allocNames(base, "Get"+base)
		fieldName, fieldGetterName := ns[0], ns[1]
		typename, wiretype := g.GoType(message, field)
		jsonName := *field.Name
		tag := fmt.Sprintf("protobuf:%s json:%q", g.goTag(message, field, wiretype), jsonName+",omitempty")
		// 保存字段与字段名、字段getter方法名之间的映射关系
		fieldNames[field] = fieldName
		fieldGetterNames[field] = fieldGetterName

		oneof := field.OneofIndex != nil
		if oneof && oneofFieldName[*field.OneofIndex] == "" {
			odp := message.OneofDecl[int(*field.OneofIndex)]
			fname := allocNames(CamelCase(odp.GetName()))[0]

			// This is the first field of a oneof we haven't seen before.
			// Generate the union field.
			com := g.PrintComments(fmt.Sprintf("%s,%d,%d", message.path, messageOneofPath, *field.OneofIndex))
			if com {
				g.P("//")
			}
			g.P("// Types that are valid to be assigned to ", fname, ":")
			// Generate the rest of this comment later,
			// when we've computed any disambiguation.
			oneofInsertPoints[*field.OneofIndex] = g.Buffer.Len()

			dname := "is" + ccTypeName + "_" + fname
			oneofFieldName[*field.OneofIndex] = fname
			oneofDisc[*field.OneofIndex] = dname
			tag := `protobuf_oneof:"` + odp.GetName() + `"`
			g.P(fname, " ", dname, " `", tag, "`")
		}

		if *field.Type == descriptor.FieldDescriptorProto_TYPE_MESSAGE {
			desc := g.ObjectNamed(field.GetTypeName())
			if d, ok := desc.(*Descriptor); ok && d.GetOptions().GetMapEntry() {
				// Figure out the Go types and tags for the key and value types.
				keyField, valField := d.Field[0], d.Field[1]
				keyType, keyWire := g.GoType(d, keyField)
				valType, valWire := g.GoType(d, valField)
				keyTag, valTag := g.goTag(d, keyField, keyWire), g.goTag(d, valField, valWire)

				// We don't use stars, except for message-typed values.
				// Message and enum types are the only two possibly foreign types used in maps,
				// so record their use. They are not permitted as map keys.
				keyType = strings.TrimPrefix(keyType, "*")
				switch *valField.Type {
				case descriptor.FieldDescriptorProto_TYPE_ENUM:
					valType = strings.TrimPrefix(valType, "*")
					g.RecordTypeUse(valField.GetTypeName())
				case descriptor.FieldDescriptorProto_TYPE_MESSAGE:
					g.RecordTypeUse(valField.GetTypeName())
				default:
					valType = strings.TrimPrefix(valType, "*")
				}

				typename = fmt.Sprintf("map[%s]%s", keyType, valType)
				mapFieldTypes[field] = typename // record for the getter generation

				tag += fmt.Sprintf(" protobuf_key:%s protobuf_val:%s", keyTag, valTag)
			}
		}

		fieldTypes[field] = typename

		if oneof {
			tname := ccTypeName + "_" + fieldName
			// It is possible for this to collide with a message or enum
			// nested in this message. Check for collisions.
			for {
				ok := true
				for _, desc := range message.nested {
					if CamelCaseSlice(desc.TypeName()) == tname {
						ok = false
						break
					}
				}
				for _, enum := range message.enums {
					if CamelCaseSlice(enum.TypeName()) == tname {
						ok = false
						break
					}
				}
				if !ok {
					tname += "_"
					continue
				}
				break
			}

			oneofTypeName[field] = tname
			continue
		}

		g.PrintComments(fmt.Sprintf("%s,%d,%d", message.path, messageFieldPath, i))
		g.P(fieldName, "\t", typename, "\t`", tag, "`")
		g.RecordTypeUse(field.GetTypeName())
	}
	if len(message.ExtensionRange) > 0 {
		g.P(g.Pkg["proto"], ".XXX_InternalExtensions `json:\"-\"`")
	}
	if !message.proto3() {
		g.P("XXX_unrecognized\t[]byte `json:\"-\"`")
	}
	g.Out()
	g.P("}")

	// Update g.Buffer to list valid oneof types.
	// We do this down here, after we've disambiguated the oneof type names.
	// We go in reverse order of insertion point to avoid invalidating offsets.
	for oi := int32(len(message.OneofDecl)); oi >= 0; oi-- {
		ip := oneofInsertPoints[oi]
		all := g.Buffer.Bytes()
		rem := all[ip:]
		g.Buffer = bytes.NewBuffer(all[:ip:ip]) // set cap so we don't scribble on rem
		for _, field := range message.Field {
			if field.OneofIndex == nil || *field.OneofIndex != oi {
				continue
			}
			g.P("//\t*", oneofTypeName[field])
		}
		g.Buffer.Write(rem)
	}

	// Reset, String and ProtoMessage methods.
	g.P("func (m *", ccTypeName, ") Reset() { *m = ", ccTypeName, "{} }")
	g.P("func (m *", ccTypeName, ") String() string { return ", g.Pkg["proto"], ".CompactTextString(m) }")
	g.P("func (*", ccTypeName, ") ProtoMessage() {}")
	var indexes []string
	for m := message; m != nil; m = m.parent {
		indexes = append([]string{strconv.Itoa(m.index)}, indexes...)
	}
	g.P("func (*", ccTypeName, ") Descriptor() ([]byte, []int) { return ", g.file.VarName(), ", []int{", strings.Join(indexes, ", "), "} }")
	// TODO: Revisit the decision to use a XXX_WellKnownType method
	// if we change proto.MessageName to work with multiple equivalents.
	if message.file.GetPackage() == "google.protobuf" && wellKnownTypes[message.GetName()] {
		g.P("func (*", ccTypeName, `) XXX_WellKnownType() string { return "`, message.GetName(), `" }`)
	}

	// Extension support methods
	var hasExtensions, isMessageSet bool
	if len(message.ExtensionRange) > 0 {
		hasExtensions = true
		// message_set_wire_format only makes sense when extensions are defined.
		if opts := message.Options; opts != nil && opts.GetMessageSetWireFormat() {
			isMessageSet = true
			g.P()
			g.P("func (m *", ccTypeName, ") Marshal() ([]byte, error) {")
			g.In()
			g.P("return ", g.Pkg["proto"], ".MarshalMessageSet(&m.XXX_InternalExtensions)")
			g.Out()
			g.P("}")
			g.P("func (m *", ccTypeName, ") Unmarshal(buf []byte) error {")
			g.In()
			g.P("return ", g.Pkg["proto"], ".UnmarshalMessageSet(buf, &m.XXX_InternalExtensions)")
			g.Out()
			g.P("}")
			g.P("func (m *", ccTypeName, ") MarshalJSON() ([]byte, error) {")
			g.In()
			g.P("return ", g.Pkg["proto"], ".MarshalMessageSetJSON(&m.XXX_InternalExtensions)")
			g.Out()
			g.P("}")
			g.P("func (m *", ccTypeName, ") UnmarshalJSON(buf []byte) error {")
			g.In()
			g.P("return ", g.Pkg["proto"], ".UnmarshalMessageSetJSON(buf, &m.XXX_InternalExtensions)")
			g.Out()
			g.P("}")
			g.P("// ensure ", ccTypeName, " satisfies proto.Marshaler and proto.Unmarshaler")
			g.P("var _ ", g.Pkg["proto"], ".Marshaler = (*", ccTypeName, ")(nil)")
			g.P("var _ ", g.Pkg["proto"], ".Unmarshaler = (*", ccTypeName, ")(nil)")
		}

		g.P()
		g.P("var extRange_", ccTypeName, " = []", g.Pkg["proto"], ".ExtensionRange{")
		g.In()
		for _, r := range message.ExtensionRange {
			end := fmt.Sprint(*r.End - 1) // make range inclusive on both ends
			g.P("{", r.Start, ", ", end, "},")
		}
		g.Out()
		g.P("}")
		g.P("func (*", ccTypeName, ") ExtensionRangeArray() []", g.Pkg["proto"], ".ExtensionRange {")
		g.In()
		g.P("return extRange_", ccTypeName)
		g.Out()
		g.P("}")
	}

	// Default constants
	defNames := make(map[*descriptor.FieldDescriptorProto]string)
	for _, field := range message.Field {
		def := field.GetDefaultValue()
		if def == "" {
			continue
		}
		fieldname := "Default_" + ccTypeName + "_" + CamelCase(*field.Name)
		defNames[field] = fieldname
		typename, _ := g.GoType(message, field)
		if typename[0] == '*' {
			typename = typename[1:]
		}
		kind := "const "
		switch {
		case typename == "bool":
		case typename == "string":
			def = strconv.Quote(def)
		case typename == "[]byte":
			def = "[]byte(" + strconv.Quote(def) + ")"
			kind = "var "
		case def == "inf", def == "-inf", def == "nan":
			// These names are known to, and defined by, the protocol language.
			switch def {
			case "inf":
				def = "math.Inf(1)"
			case "-inf":
				def = "math.Inf(-1)"
			case "nan":
				def = "math.NaN()"
			}
			if *field.Type == descriptor.FieldDescriptorProto_TYPE_FLOAT {
				def = "float32(" + def + ")"
			}
			kind = "var "
		case *field.Type == descriptor.FieldDescriptorProto_TYPE_ENUM:
			// Must be an enum.  Need to construct the prefixed name.
			obj := g.ObjectNamed(field.GetTypeName())
			var enum *EnumDescriptor
			if id, ok := obj.(*ImportedDescriptor); ok {
				// The enum type has been publicly imported.
				enum, _ = id.o.(*EnumDescriptor)
			} else {
				enum, _ = obj.(*EnumDescriptor)
			}
			if enum == nil {
				log.Printf("don't know how to generate constant for %s", fieldname)
				continue
			}
			def = g.DefaultPackageName(obj) + enum.prefix() + def
		}
		g.P(kind, fieldname, " ", typename, " = ", def)
		g.file.addExport(message, constOrVarSymbol{fieldname, kind, ""})
	}
	g.P()

	// Oneof per-field types, discriminants and getters.
	//
	// Generate unexported named types for the discriminant interfaces.
	// We shouldn't have to do this, but there was (~19 Aug 2015) a compiler/linker bug
	// that was triggered by using anonymous interfaces here.
	// TODO: Revisit this and consider reverting back to anonymous interfaces.
	for oi := range message.OneofDecl {
		dname := oneofDisc[int32(oi)]
		g.P("type ", dname, " interface { ", dname, "() }")
	}
	g.P()
	for _, field := range message.Field {
		if field.OneofIndex == nil {
			continue
		}
		_, wiretype := g.GoType(message, field)
		tag := "protobuf:" + g.goTag(message, field, wiretype)
		g.P("type ", oneofTypeName[field], " struct{ ", fieldNames[field], " ", fieldTypes[field], " `", tag, "` }")
		g.RecordTypeUse(field.GetTypeName())
	}
	g.P()
	for _, field := range message.Field {
		if field.OneofIndex == nil {
			continue
		}
		g.P("func (*", oneofTypeName[field], ") ", oneofDisc[*field.OneofIndex], "() {}")
	}
	g.P()
	for oi := range message.OneofDecl {
		fname := oneofFieldName[int32(oi)]
		g.P("func (m *", ccTypeName, ") Get", fname, "() ", oneofDisc[int32(oi)], " {")
		g.P("if m != nil { return m.", fname, " }")
		g.P("return nil")
		g.P("}")
	}
	g.P()

	// Field getters
	var getters []getterSymbol
	for _, field := range message.Field {
		oneof := field.OneofIndex != nil

		fname := fieldNames[field]
		typename, _ := g.GoType(message, field)
		if t, ok := mapFieldTypes[field]; ok {
			typename = t
		}
		mname := fieldGetterNames[field]
		star := ""
		if needsStar(*field.Type) && typename[0] == '*' {
			typename = typename[1:]
			star = "*"
		}

		// Only export getter symbols for basic types,
		// and for messages and enums in the same package.
		// Groups are not exported.
		// Foreign types can't be hoisted through a public import because
		// the importer may not already be importing the defining .proto.
		// As an example, imagine we have an import tree like this:
		//   A.proto -> B.proto -> C.proto
		// If A publicly imports B, we need to generate the getters from B in A's output,
		// but if one such getter returns something from C then we cannot do that
		// because A is not importing C already.
		var getter, genType bool
		switch *field.Type {
		case descriptor.FieldDescriptorProto_TYPE_GROUP:
			getter = false
		case descriptor.FieldDescriptorProto_TYPE_MESSAGE, descriptor.FieldDescriptorProto_TYPE_ENUM:
			// Only export getter if its return type is in this package.
			getter = g.ObjectNamed(field.GetTypeName()).PackageName() == message.PackageName()
			genType = true
		default:
			getter = true
		}
		if getter {
			getters = append(getters, getterSymbol{
				name:     mname,
				typ:      typename,
				typeName: field.GetTypeName(),
				genType:  genType,
			})
		}

		g.P("func (m *", ccTypeName, ") "+mname+"() "+typename+" {")
		g.In()
		def, hasDef := defNames[field]
		typeDefaultIsNil := false // whether this field type's default value is a literal nil unless specified
		switch *field.Type {
		case descriptor.FieldDescriptorProto_TYPE_BYTES:
			typeDefaultIsNil = !hasDef
		case descriptor.FieldDescriptorProto_TYPE_GROUP, descriptor.FieldDescriptorProto_TYPE_MESSAGE:
			typeDefaultIsNil = true
		}
		if isRepeated(field) {
			typeDefaultIsNil = true
		}
		if typeDefaultIsNil && !oneof {
			// A bytes field with no explicit default needs less generated code,
			// as does a message or group field, or a repeated field.
			g.P("if m != nil {")
			g.In()
			g.P("return m." + fname)
			g.Out()
			g.P("}")
			g.P("return nil")
			g.Out()
			g.P("}")
			g.P()
			continue
		}
		if !oneof {
			if message.proto3() {
				g.P("if m != nil {")
			} else {
				g.P("if m != nil && m." + fname + " != nil {")
			}
			g.In()
			g.P("return " + star + "m." + fname)
			g.Out()
			g.P("}")
		} else {
			uname := oneofFieldName[*field.OneofIndex]
			tname := oneofTypeName[field]
			g.P("if x, ok := m.Get", uname, "().(*", tname, "); ok {")
			g.P("return x.", fname)
			g.P("}")
		}
		if hasDef {
			if *field.Type != descriptor.FieldDescriptorProto_TYPE_BYTES {
				g.P("return " + def)
			} else {
				// The default is a []byte var.
				// Make a copy when returning it to be safe.
				g.P("return append([]byte(nil), ", def, "...)")
			}
		} else {
			switch *field.Type {
			case descriptor.FieldDescriptorProto_TYPE_BOOL:
				g.P("return false")
			case descriptor.FieldDescriptorProto_TYPE_STRING:
				g.P(`return ""`)
			case descriptor.FieldDescriptorProto_TYPE_GROUP,
				descriptor.FieldDescriptorProto_TYPE_MESSAGE,
				descriptor.FieldDescriptorProto_TYPE_BYTES:
				// This is only possible for oneof fields.
				g.P("return nil")
			case descriptor.FieldDescriptorProto_TYPE_ENUM:
				// The default default for an enum is the first value in the enum,
				// not zero.
				obj := g.ObjectNamed(field.GetTypeName())
				var enum *EnumDescriptor
				if id, ok := obj.(*ImportedDescriptor); ok {
					// The enum type has been publicly imported.
					enum, _ = id.o.(*EnumDescriptor)
				} else {
					enum, _ = obj.(*EnumDescriptor)
				}
				if enum == nil {
					log.Printf("don't know how to generate getter for %s", field.GetName())
					continue
				}
				if len(enum.Value) == 0 {
					g.P("return 0 // empty enum")
				} else {
					first := enum.Value[0].GetName()
					g.P("return ", g.DefaultPackageName(obj)+enum.prefix()+first)
				}
			default:
				g.P("return 0")
			}
		}
		g.Out()
		g.P("}")
		g.P()
	}

	if !message.group {
		ms := &messageSymbol{
			sym:           ccTypeName,
			hasExtensions: hasExtensions,
			isMessageSet:  isMessageSet,
			hasOneof:      len(message.OneofDecl) > 0,
			getters:       getters,
		}
		g.file.addExport(message, ms)
	}

	// Oneof functions
	if len(message.OneofDecl) > 0 {
		fieldWire := make(map[*descriptor.FieldDescriptorProto]string)

		// method
		enc := "_" + ccTypeName + "_OneofMarshaler"
		dec := "_" + ccTypeName + "_OneofUnmarshaler"
		size := "_" + ccTypeName + "_OneofSizer"
		encSig := "(msg " + g.Pkg["proto"] + ".Message, b *" + g.Pkg["proto"] + ".Buffer) error"
		decSig := "(msg " + g.Pkg["proto"] + ".Message, tag, wire int, b *" + g.Pkg["proto"] + ".Buffer) (bool, error)"
		sizeSig := "(msg " + g.Pkg["proto"] + ".Message) (n int)"

		g.P("// XXX_OneofFuncs is for the internal use of the proto package.")
		g.P("func (*", ccTypeName, ") XXX_OneofFuncs() (func", encSig, ", func", decSig, ", func", sizeSig, ", []interface{}) {")
		g.P("return ", enc, ", ", dec, ", ", size, ", []interface{}{")
		for _, field := range message.Field {
			if field.OneofIndex == nil {
				continue
			}
			g.P("(*", oneofTypeName[field], ")(nil),")
		}
		g.P("}")
		g.P("}")
		g.P()

		// marshaler
		g.P("func ", enc, encSig, " {")
		g.P("m := msg.(*", ccTypeName, ")")
		for oi, odp := range message.OneofDecl {
			g.P("// ", odp.GetName())
			fname := oneofFieldName[int32(oi)]
			g.P("switch x := m.", fname, ".(type) {")
			for _, field := range message.Field {
				if field.OneofIndex == nil || int(*field.OneofIndex) != oi {
					continue
				}
				g.P("case *", oneofTypeName[field], ":")
				var wire, pre, post string
				val := "x." + fieldNames[field] // overridden for TYPE_BOOL
				canFail := false                // only TYPE_MESSAGE and TYPE_GROUP can fail
				switch *field.Type {
				case descriptor.FieldDescriptorProto_TYPE_DOUBLE:
					wire = "WireFixed64"
					pre = "b.EncodeFixed64(" + g.Pkg["math"] + ".Float64bits("
					post = "))"
				case descriptor.FieldDescriptorProto_TYPE_FLOAT:
					wire = "WireFixed32"
					pre = "b.EncodeFixed32(uint64(" + g.Pkg["math"] + ".Float32bits("
					post = ")))"
				case descriptor.FieldDescriptorProto_TYPE_INT64,
					descriptor.FieldDescriptorProto_TYPE_UINT64:
					wire = "WireVarint"
					pre, post = "b.EncodeVarint(uint64(", "))"
				case descriptor.FieldDescriptorProto_TYPE_INT32,
					descriptor.FieldDescriptorProto_TYPE_UINT32,
					descriptor.FieldDescriptorProto_TYPE_ENUM:
					wire = "WireVarint"
					pre, post = "b.EncodeVarint(uint64(", "))"
				case descriptor.FieldDescriptorProto_TYPE_FIXED64,
					descriptor.FieldDescriptorProto_TYPE_SFIXED64:
					wire = "WireFixed64"
					pre, post = "b.EncodeFixed64(uint64(", "))"
				case descriptor.FieldDescriptorProto_TYPE_FIXED32,
					descriptor.FieldDescriptorProto_TYPE_SFIXED32:
					wire = "WireFixed32"
					pre, post = "b.EncodeFixed32(uint64(", "))"
				case descriptor.FieldDescriptorProto_TYPE_BOOL:
					// bool needs special handling.
					g.P("t := uint64(0)")
					g.P("if ", val, " { t = 1 }")
					val = "t"
					wire = "WireVarint"
					pre, post = "b.EncodeVarint(", ")"
				case descriptor.FieldDescriptorProto_TYPE_STRING:
					wire = "WireBytes"
					pre, post = "b.EncodeStringBytes(", ")"
				case descriptor.FieldDescriptorProto_TYPE_GROUP:
					wire = "WireStartGroup"
					pre, post = "b.Marshal(", ")"
					canFail = true
				case descriptor.FieldDescriptorProto_TYPE_MESSAGE:
					wire = "WireBytes"
					pre, post = "b.EncodeMessage(", ")"
					canFail = true
				case descriptor.FieldDescriptorProto_TYPE_BYTES:
					wire = "WireBytes"
					pre, post = "b.EncodeRawBytes(", ")"
				case descriptor.FieldDescriptorProto_TYPE_SINT32:
					wire = "WireVarint"
					pre, post = "b.EncodeZigzag32(uint64(", "))"
				case descriptor.FieldDescriptorProto_TYPE_SINT64:
					wire = "WireVarint"
					pre, post = "b.EncodeZigzag64(uint64(", "))"
				default:
					g.Fail("unhandled oneof field type ", field.Type.String())
				}
				fieldWire[field] = wire
				g.P("b.EncodeVarint(", field.Number, "<<3|", g.Pkg["proto"], ".", wire, ")")
				if !canFail {
					g.P(pre, val, post)
				} else {
					g.P("if err := ", pre, val, post, "; err != nil {")
					g.P("return err")
					g.P("}")
				}
				if *field.Type == descriptor.FieldDescriptorProto_TYPE_GROUP {
					g.P("b.EncodeVarint(", field.Number, "<<3|", g.Pkg["proto"], ".WireEndGroup)")
				}
			}
			g.P("case nil:")
			g.P("default: return ", g.Pkg["fmt"], `.Errorf("`, ccTypeName, ".", fname, ` has unexpected type %T", x)`)
			g.P("}")
		}
		g.P("return nil")
		g.P("}")
		g.P()

		// unmarshaler
		g.P("func ", dec, decSig, " {")
		g.P("m := msg.(*", ccTypeName, ")")
		g.P("switch tag {")
		for _, field := range message.Field {
			if field.OneofIndex == nil {
				continue
			}
			odp := message.OneofDecl[int(*field.OneofIndex)]
			g.P("case ", field.Number, ": // ", odp.GetName(), ".", *field.Name)
			g.P("if wire != ", g.Pkg["proto"], ".", fieldWire[field], " {")
			g.P("return true, ", g.Pkg["proto"], ".ErrInternalBadWireType")
			g.P("}")
			lhs := "x, err" // overridden for TYPE_MESSAGE and TYPE_GROUP
			var dec, cast, cast2 string
			switch *field.Type {
			case descriptor.FieldDescriptorProto_TYPE_DOUBLE:
				dec, cast = "b.DecodeFixed64()", g.Pkg["math"]+".Float64frombits"
			case descriptor.FieldDescriptorProto_TYPE_FLOAT:
				dec, cast, cast2 = "b.DecodeFixed32()", "uint32", g.Pkg["math"]+".Float32frombits"
			case descriptor.FieldDescriptorProto_TYPE_INT64:
				dec, cast = "b.DecodeVarint()", "int64"
			case descriptor.FieldDescriptorProto_TYPE_UINT64:
				dec = "b.DecodeVarint()"
			case descriptor.FieldDescriptorProto_TYPE_INT32:
				dec, cast = "b.DecodeVarint()", "int32"
			case descriptor.FieldDescriptorProto_TYPE_FIXED64:
				dec = "b.DecodeFixed64()"
			case descriptor.FieldDescriptorProto_TYPE_FIXED32:
				dec, cast = "b.DecodeFixed32()", "uint32"
			case descriptor.FieldDescriptorProto_TYPE_BOOL:
				dec = "b.DecodeVarint()"
				// handled specially below
			case descriptor.FieldDescriptorProto_TYPE_STRING:
				dec = "b.DecodeStringBytes()"
			case descriptor.FieldDescriptorProto_TYPE_GROUP:
				g.P("msg := new(", fieldTypes[field][1:], ")") // drop star
				lhs = "err"
				dec = "b.DecodeGroup(msg)"
				// handled specially below
			case descriptor.FieldDescriptorProto_TYPE_MESSAGE:
				g.P("msg := new(", fieldTypes[field][1:], ")") // drop star
				lhs = "err"
				dec = "b.DecodeMessage(msg)"
				// handled specially below
			case descriptor.FieldDescriptorProto_TYPE_BYTES:
				dec = "b.DecodeRawBytes(true)"
			case descriptor.FieldDescriptorProto_TYPE_UINT32:
				dec, cast = "b.DecodeVarint()", "uint32"
			case descriptor.FieldDescriptorProto_TYPE_ENUM:
				dec, cast = "b.DecodeVarint()", fieldTypes[field]
			case descriptor.FieldDescriptorProto_TYPE_SFIXED32:
				dec, cast = "b.DecodeFixed32()", "int32"
			case descriptor.FieldDescriptorProto_TYPE_SFIXED64:
				dec, cast = "b.DecodeFixed64()", "int64"
			case descriptor.FieldDescriptorProto_TYPE_SINT32:
				dec, cast = "b.DecodeZigzag32()", "int32"
			case descriptor.FieldDescriptorProto_TYPE_SINT64:
				dec, cast = "b.DecodeZigzag64()", "int64"
			default:
				g.Fail("unhandled oneof field type ", field.Type.String())
			}
			g.P(lhs, " := ", dec)
			val := "x"
			if cast != "" {
				val = cast + "(" + val + ")"
			}
			if cast2 != "" {
				val = cast2 + "(" + val + ")"
			}
			switch *field.Type {
			case descriptor.FieldDescriptorProto_TYPE_BOOL:
				val += " != 0"
			case descriptor.FieldDescriptorProto_TYPE_GROUP,
				descriptor.FieldDescriptorProto_TYPE_MESSAGE:
				val = "msg"
			}
			g.P("m.", oneofFieldName[*field.OneofIndex], " = &", oneofTypeName[field], "{", val, "}")
			g.P("return true, err")
		}
		g.P("default: return false, nil")
		g.P("}")
		g.P("}")
		g.P()

		// sizer
		g.P("func ", size, sizeSig, " {")
		g.P("m := msg.(*", ccTypeName, ")")
		for oi, odp := range message.OneofDecl {
			g.P("// ", odp.GetName())
			fname := oneofFieldName[int32(oi)]
			g.P("switch x := m.", fname, ".(type) {")
			for _, field := range message.Field {
				if field.OneofIndex == nil || int(*field.OneofIndex) != oi {
					continue
				}
				g.P("case *", oneofTypeName[field], ":")
				val := "x." + fieldNames[field]
				var wire, varint, fixed string
				switch *field.Type {
				case descriptor.FieldDescriptorProto_TYPE_DOUBLE:
					wire = "WireFixed64"
					fixed = "8"
				case descriptor.FieldDescriptorProto_TYPE_FLOAT:
					wire = "WireFixed32"
					fixed = "4"
				case descriptor.FieldDescriptorProto_TYPE_INT64,
					descriptor.FieldDescriptorProto_TYPE_UINT64,
					descriptor.FieldDescriptorProto_TYPE_INT32,
					descriptor.FieldDescriptorProto_TYPE_UINT32,
					descriptor.FieldDescriptorProto_TYPE_ENUM:
					wire = "WireVarint"
					varint = val
				case descriptor.FieldDescriptorProto_TYPE_FIXED64,
					descriptor.FieldDescriptorProto_TYPE_SFIXED64:
					wire = "WireFixed64"
					fixed = "8"
				case descriptor.FieldDescriptorProto_TYPE_FIXED32,
					descriptor.FieldDescriptorProto_TYPE_SFIXED32:
					wire = "WireFixed32"
					fixed = "4"
				case descriptor.FieldDescriptorProto_TYPE_BOOL:
					wire = "WireVarint"
					fixed = "1"
				case descriptor.FieldDescriptorProto_TYPE_STRING:
					wire = "WireBytes"
					fixed = "len(" + val + ")"
					varint = fixed
				case descriptor.FieldDescriptorProto_TYPE_GROUP:
					wire = "WireStartGroup"
					fixed = g.Pkg["proto"] + ".Size(" + val + ")"
				case descriptor.FieldDescriptorProto_TYPE_MESSAGE:
					wire = "WireBytes"
					g.P("s := ", g.Pkg["proto"], ".Size(", val, ")")
					fixed = "s"
					varint = fixed
				case descriptor.FieldDescriptorProto_TYPE_BYTES:
					wire = "WireBytes"
					fixed = "len(" + val + ")"
					varint = fixed
				case descriptor.FieldDescriptorProto_TYPE_SINT32:
					wire = "WireVarint"
					varint = "(uint32(" + val + ") << 1) ^ uint32((int32(" + val + ") >> 31))"
				case descriptor.FieldDescriptorProto_TYPE_SINT64:
					wire = "WireVarint"
					varint = "uint64(" + val + " << 1) ^ uint64((int64(" + val + ") >> 63))"
				default:
					g.Fail("unhandled oneof field type ", field.Type.String())
				}
				g.P("n += ", g.Pkg["proto"], ".SizeVarint(", field.Number, "<<3|", g.Pkg["proto"], ".", wire, ")")
				if varint != "" {
					g.P("n += ", g.Pkg["proto"], ".SizeVarint(uint64(", varint, "))")
				}
				if fixed != "" {
					g.P("n += ", fixed)
				}
				if *field.Type == descriptor.FieldDescriptorProto_TYPE_GROUP {
					g.P("n += ", g.Pkg["proto"], ".SizeVarint(", field.Number, "<<3|", g.Pkg["proto"], ".WireEndGroup)")
				}
			}
			g.P("case nil:")
			g.P("default:")
			g.P("panic(", g.Pkg["fmt"], ".Sprintf(\"proto: unexpected type %T in oneof\", x))")
			g.P("}")
		}
		g.P("return n")
		g.P("}")
		g.P()
	}

	for _, ext := range message.ext {
		g.generateExtension(ext)
	}

	fullName := strings.Join(message.TypeName(), ".")
	if g.file.Package != nil {
		fullName = *g.file.Package + "." + fullName
	}

	g.addInitf("%s.RegisterType((*%s)(nil), %q)", g.Pkg["proto"], ccTypeName, fullName)
}

func (g *Generator) generateExtension(ext *ExtensionDescriptor) {
	ccTypeName := ext.DescName()

	extObj := g.ObjectNamed(*ext.Extendee)
	var extDesc *Descriptor
	if id, ok := extObj.(*ImportedDescriptor); ok {
		// This is extending a publicly imported message.
		// We need the underlying type for goTag.
		extDesc = id.o.(*Descriptor)
	} else {
		extDesc = extObj.(*Descriptor)
	}
	extendedType := "*" + g.TypeName(extObj) // always use the original
	field := ext.FieldDescriptorProto
	fieldType, wireType := g.GoType(ext.parent, field)
	tag := g.goTag(extDesc, field, wireType)
	g.RecordTypeUse(*ext.Extendee)
	if n := ext.FieldDescriptorProto.TypeName; n != nil {
		// foreign extension type
		g.RecordTypeUse(*n)
	}

	typeName := ext.TypeName()

	// Special case for proto2 message sets: If this extension is extending
	// proto2_bridge.MessageSet, and its final name component is "message_set_extension",
	// then drop that last component.
	mset := false
	if extendedType == "*proto2_bridge.MessageSet" && typeName[len(typeName)-1] == "message_set_extension" {
		typeName = typeName[:len(typeName)-1]
		mset = true
	}

	// For text formatting, the package must be exactly what the .proto file declares,
	// ignoring overrides such as the go_package option, and with no dot/underscore mapping.
	extName := strings.Join(typeName, ".")
	if g.file.Package != nil {
		extName = *g.file.Package + "." + extName
	}

	g.P("var ", ccTypeName, " = &", g.Pkg["proto"], ".ExtensionDesc{")
	g.In()
	g.P("ExtendedType: (", extendedType, ")(nil),")
	g.P("ExtensionType: (", fieldType, ")(nil),")
	g.P("Field: ", field.Number, ",")
	g.P(`Name: "`, extName, `",`)
	g.P("Tag: ", tag, ",")
	g.P(`Filename: "`, g.file.GetName(), `",`)

	g.Out()
	g.P("}")
	g.P()

	if mset {
		// Generate a bit more code to register with message_set.go.
		g.addInitf("%s.RegisterMessageSetType((%s)(nil), %d, %q)", g.Pkg["proto"], fieldType, *field.Number, extName)
	}

	g.file.addExport(ext, constOrVarSymbol{ccTypeName, "var", ""})
}

func (g *Generator) generateInitFunction() {
	for _, enum := range g.file.enum {
		g.generateEnumRegistration(enum)
	}
	for _, d := range g.file.desc {
		for _, ext := range d.ext {
			g.generateExtensionRegistration(ext)
		}
	}
	for _, ext := range g.file.ext {
		g.generateExtensionRegistration(ext)
	}
	if len(g.init) == 0 {
		return
	}
	g.P("func init() {")
	g.In()
	for _, l := range g.init {
		g.P(l)
	}
	g.Out()
	g.P("}")
	g.init = nil
}

func (g *Generator) generateFileDescriptor(file *FileDescriptor) {
	// Make a copy and trim source_code_info data.
	// TODO: Trim this more when we know exactly what we need.
	pb := proto.Clone(file.FileDescriptorProto).(*descriptor.FileDescriptorProto)
	pb.SourceCodeInfo = nil

	b, err := proto.Marshal(pb)
	if err != nil {
		g.Fail(err.Error())
	}

	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	w.Write(b)
	w.Close()
	b = buf.Bytes()

	v := file.VarName()
	g.P()
	g.P("func init() { ", g.Pkg["proto"], ".RegisterFile(", strconv.Quote(*file.Name), ", ", v, ") }")
	g.P("var ", v, " = []byte{")
	g.In()
	g.P("// ", len(b), " bytes of a gzipped FileDescriptorProto")
	for len(b) > 0 {
		n := 16
		if n > len(b) {
			n = len(b)
		}

		s := ""
		for _, c := range b[:n] {
			s += fmt.Sprintf("0x%02x,", c)
		}
		g.P(s)

		b = b[n:]
	}
	g.Out()
	g.P("}")
}

func (g *Generator) generateEnumRegistration(enum *EnumDescriptor) {
	// // We always print the full (proto-world) package name here.
	pkg := enum.File().GetPackage()
	if pkg != "" {
		pkg += "."
	}
	// The full type name
	typeName := enum.TypeName()
	// The full type name, CamelCased.
	ccTypeName := CamelCaseSlice(typeName)
	g.addInitf("%s.RegisterEnum(%q, %[3]s_name, %[3]s_value)", g.Pkg["proto"], pkg+ccTypeName, ccTypeName)
}

func (g *Generator) generateExtensionRegistration(ext *ExtensionDescriptor) {
	g.addInitf("%s.RegisterExtension(%s)", g.Pkg["proto"], ext.DescName())
}

// And now lots of helper functions.

// Is c an ASCII lower-case letter?
func isASCIILower(c byte) bool {
	return 'a' <= c && c <= 'z'
}

// Is c an ASCII digit?
func isASCIIDigit(c byte) bool {
	return '0' <= c && c <= '9'
}

// CamelCase returns the CamelCased name.
// If there is an interior underscore followed by a lower case letter,
// drop the underscore and convert the letter to upper case.
// There is a remote possibility of this rewrite causing a name collision,
// but it's so remote we're prepared to pretend it's nonexistent - since the
// C++ generator lowercases names, it's extremely unlikely to have two fields
// with different capitalizations.
// In short, _my_field_name_2 becomes XMyFieldName_2.
func CamelCase(s string) string {
	if s == "" {
		return ""
	}
	t := make([]byte, 0, 32)
	i := 0
	if s[0] == '_' {
		// Need a capital letter; drop the '_'.
		t = append(t, 'X')
		i++
	}
	// Invariant: if the next letter is lower case, it must be converted
	// to upper case.
	// That is, we process a word at a time, where words are marked by _ or
	// upper case letter. Digits are treated as words.
	for ; i < len(s); i++ {
		c := s[i]
		if c == '_' && i+1 < len(s) && isASCIILower(s[i+1]) {
			continue // Skip the underscore in s.
		}
		if isASCIIDigit(c) {
			t = append(t, c)
			continue
		}
		// Assume we have a letter now - if not, it's a bogus identifier.
		// The next word is a sequence of characters that must start upper case.
		if isASCIILower(c) {
			c ^= ' ' // Make it a capital letter.
		}
		t = append(t, c) // Guaranteed not lower case.
		// Accept lower case sequence that follows.
		for i+1 < len(s) && isASCIILower(s[i+1]) {
			i++
			t = append(t, s[i])
		}
	}
	return string(t)
}

// CamelCaseSlice is like CamelCase, but the argument is a slice of strings to
// be joined with "_".
func CamelCaseSlice(elem []string) string { return CamelCase(strings.Join(elem, "_")) }

// dottedSlice turns a sliced name into a dotted name.
func dottedSlice(elem []string) string { return strings.Join(elem, ".") }

// Is this field optional?
func isOptional(field *descriptor.FieldDescriptorProto) bool {
	return field.Label != nil && *field.Label == descriptor.FieldDescriptorProto_LABEL_OPTIONAL
}

// Is this field required?
func isRequired(field *descriptor.FieldDescriptorProto) bool {
	return field.Label != nil && *field.Label == descriptor.FieldDescriptorProto_LABEL_REQUIRED
}

// Is this field repeated?
func isRepeated(field *descriptor.FieldDescriptorProto) bool {
	return field.Label != nil && *field.Label == descriptor.FieldDescriptorProto_LABEL_REPEATED
}

// Is this field a scalar numeric type?
func isScalar(field *descriptor.FieldDescriptorProto) bool {
	if field.Type == nil {
		return false
	}
	switch *field.Type {
	case descriptor.FieldDescriptorProto_TYPE_DOUBLE,
		descriptor.FieldDescriptorProto_TYPE_FLOAT,
		descriptor.FieldDescriptorProto_TYPE_INT64,
		descriptor.FieldDescriptorProto_TYPE_UINT64,
		descriptor.FieldDescriptorProto_TYPE_INT32,
		descriptor.FieldDescriptorProto_TYPE_FIXED64,
		descriptor.FieldDescriptorProto_TYPE_FIXED32,
		descriptor.FieldDescriptorProto_TYPE_BOOL,
		descriptor.FieldDescriptorProto_TYPE_UINT32,
		descriptor.FieldDescriptorProto_TYPE_ENUM,
		descriptor.FieldDescriptorProto_TYPE_SFIXED32,
		descriptor.FieldDescriptorProto_TYPE_SFIXED64,
		descriptor.FieldDescriptorProto_TYPE_SINT32,
		descriptor.FieldDescriptorProto_TYPE_SINT64:
		return true
	default:
		return false
	}
}

// badToUnderscore is the mapping function used to generate Go names from package names,
// which can be dotted in the input .proto file.  It replaces non-identifier characters such as
// dot or dash with underscore.
func badToUnderscore(r rune) rune {
	if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
		return r
	}
	return '_'
}

// baseName returns the last path element of the name, with the last dotted suffix removed.
func baseName(name string) string {
	// First, find the last element
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	// Now drop the suffix
	if i := strings.LastIndex(name, "."); i >= 0 {
		name = name[0:i]
	}
	return name
}

// The SourceCodeInfo message describes the location of elements of a parsed
// .proto file by way of a "path", which is a sequence of integers that
// describe the route from a FileDescriptorProto to the relevant submessage.
// The path alternates between a field number of a repeated field, and an index
// into that repeated field. The constants below define the field numbers that
// are used.
//
// See descriptor.proto for more information about this.
const (
	// tag numbers in FileDescriptorProto
	packagePath = 2 // package
	messagePath = 4 // message_type
	enumPath    = 5 // enum_type
	// tag numbers in DescriptorProto
	messageFieldPath   = 2 // field
	messageMessagePath = 3 // nested_type
	messageEnumPath    = 4 // enum_type
	messageOneofPath   = 8 // oneof_decl
	// tag numbers in EnumDescriptorProto
	enumValuePath = 2 // value
)
