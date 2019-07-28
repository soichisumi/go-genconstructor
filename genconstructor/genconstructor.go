/*
genconstructor is constructor generator for Go.

```go
    //genconstructor
    type Foo struct {
        key string `required:"[constValue]"`
    }
```

with `go generate` command

```go
    //go:generate go-genconstructor
```
*/
package genconstructor

import (
	"bytes"
	"go/ast"
	"go/format"
	"io"
	"os"
	"reflect"
	"strings"
	"text/template"
	"unicode"

	"github.com/GuiltyMorishita/go-genutil/genutil"
	"github.com/hori-ryota/go-strcase"
)

const (
	commentMarker = "//genconstructor"
	pointerOpts   = "-p"
	superOpts     = "-s"
	extendsOpts   = "-e"
)

type Option func(o *option)

type option struct {
	fileFilter    func(finfo os.FileInfo) bool
	generatorName string
}

func WithFileFilter(fileFilter func(finfo os.FileInfo) bool) Option {
	return func(o *option) {
		o.fileFilter = fileFilter
	}
}

func WithGeneratorName(generatorName string) Option {
	return func(o *option) {
		o.generatorName = generatorName
	}
}

func Run(targetDir string, newWriter func(pkg *ast.Package) io.Writer, opts ...Option) error {
	option := option{
		generatorName: "go-genconstructor",
	}
	for _, opt := range opts {
		opt(&option)
	}

	walkers, err := genutil.DirToAstWalker(targetDir, option.fileFilter)
	if err != nil {
		return err
	}

	for _, walker := range walkers {
		body := new(bytes.Buffer)
		importPackages := make(map[string]string, 10)
		for _, spec := range walker.AllStructSpecs() {
			docs := make([]*ast.Comment, 0, 10)
			if spec.Doc != nil {
				docs = append(docs, spec.Doc.List...)
			}
			if decl := walker.TypeSpecToGenDecl(spec); decl.Doc != nil {
				docs = append(docs, decl.Doc.List...)
			}
			if len(docs) == 0 {
				continue
			}
			hasMarker := false
			hasPointerOpts := false
			hasSuperOpts := false
			hasExtendsOpts := false
			for _, comment := range docs {
				if strings.HasPrefix(strings.TrimSpace(comment.Text), commentMarker) {
					hasMarker = true
					for _, s := range strings.Fields(comment.Text) {
						if s == pointerOpts {
							hasPointerOpts = true
							break
						}
						if s == superOpts {
							hasSuperOpts = true
							break
						}
						if s == extendsOpts {
							hasExtendsOpts = true
							break
						}
					}
					break
				}
			}
			if !hasMarker {
				continue
			}

			structType := spec.Type.(*ast.StructType)

			var superName string
			fieldInfos := make([]FieldInfo, 0, len(structType.Fields.List))
			for _, field := range structType.Fields.List {
				if field.Tag == nil {
					continue
				}
				tag := reflect.StructTag(strings.Trim(field.Tag.Value, "`"))

				constValue, hasRequiredTag := tag.Lookup("required")

				_, hasSuperTag := tag.Lookup("super")
				if !hasRequiredTag && !hasSuperTag {
					continue
				}

				fieldName := genutil.ParseFieldName(field)
				typePrinter, err := walker.ToTypePrinter(field.Type)
				if err != nil {
					return err
				}

				fieldInfos = append(fieldInfos, FieldInfo{
					Type:       typePrinter.Print(walker.PkgPath),
					Name:       fieldName,
					ConstValue: constValue,
				})

				if hasSuperTag {
					superName = fieldName
				}

				// resolve imports
				if constValue != "" {
					ss := strings.FieldsFunc(constValue, func(c rune) bool {
						return !unicode.IsLetter(c) && c != '.' && c != '_' && c != '-'
					})
					for _, s := range ss {
						p, err := genutil.ToTypePrinter(
							genutil.AstFileToImportMap(walker.ToFile(field)),
							walker.PkgPath,
							s,
						)
						if err != nil {
							return err
						}
						for n, pkg := range p.ImportPkgMap(walker.PkgPath) {
							importPackages[n] = pkg
						}
					}
					continue
				}

				for n, pkg := range typePrinter.ImportPkgMap(walker.PkgPath) {
					importPackages[n] = pkg
				}
			}

			var interfaceName string
			if hasSuperOpts {
				interfaceName = strcase.ToUpperCamel(spec.Name.Name)
			}
			if hasExtendsOpts {
				matched := match(strcase.SplitIntoWords(strcase.ToUpperCamel(superName)), strcase.SplitIntoWords(strcase.ToUpperCamel(spec.Name.Name)))
				interfaceName = strings.Join(matched, "")
			}

			if err := template.Must(template.New("constructor").Funcs(map[string]interface{}{
				"ToUpperCamel": strcase.ToUpperCamel,
				"ToLowerCamel": strcase.ToLowerCamel,
			}).Parse(`
func New{{ ToUpperCamel .StructName }}(
							{{- range .Fields }}
								{{- if not .ConstValue }}
									{{ if and ($.Extends) (eq (ToUpperCamel .Name) $.InterfaceName) }}x {{ $.InterfaceName }}{{ else }}{{ ToLowerCamel .Name }} {{ .Type }}{{ end }},
								{{- end }}
							{{- end }}
						) {{ if .Pointer }}*{{ end }}{{ if or (.Super) (.Extends) }}{{ .InterfaceName }}{{ else }}{{ .StructName }}{{ end }} {
							return {{ if or (.Pointer) (.Super) (.Extends) }}&{{ end }}{{ .StructName }}{
								{{- range .Fields }}
									{{- if .ConstValue }}
										{{ .Name }}: {{ .ConstValue }},
									{{- else }}
										{{ .Name }}: {{ if and ($.Extends) (eq (ToUpperCamel .Name) $.InterfaceName) }}x.(*{{ .Name }}){{ else }}{{ ToLowerCamel .Name }}{{ end }},
									{{- end }}
								{{- end }}
							}
						}
					`)).Execute(body, tmplParam{
				StructName:    spec.Name.Name,
				InterfaceName: interfaceName,
				Fields:        fieldInfos,
				Pointer:       hasPointerOpts,
				Super:         hasSuperOpts,
				Extends:       hasExtendsOpts,
			}); err != nil {
				return err
			}
		}
		if body.Len() == 0 {
			continue
		}

		out := new(bytes.Buffer)

		err = template.Must(template.New("out").Parse(`
			// Code generated by {{ .GeneratorName }}; DO NOT EDIT.

			package {{ .PackageName }}

			{{ .ImportPackages }}

			{{ .Body }}
		`)).Execute(out, map[string]string{
			"GeneratorName":  option.generatorName,
			"PackageName":    walker.Pkg.Name,
			"ImportPackages": genutil.GoFmtImports(importPackages),
			"Body":           body.String(),
		})
		if err != nil {
			return err
		}

		str, err := format.Source(out.Bytes())
		if err != nil {
			return err
		}
		writer := newWriter(walker.Pkg)
		if closer, ok := writer.(io.Closer); ok {
			defer closer.Close()
		}
		if _, err := writer.Write(str); err != nil {
			return err
		}
	}

	return nil
}

type tmplParam struct {
	StructName    string
	InterfaceName string
	Fields        []FieldInfo
	Pointer       bool
	Super         bool
	Extends       bool
}

type FieldInfo struct {
	Type       string
	Name       string
	ConstValue string
}

func match(a, b []string) []string {
	mb := make(map[string]struct{}, len(b))
	for _, x := range b {
		mb[x] = struct{}{}
	}
	var match []string
	for _, x := range a {
		if _, found := mb[x]; found {
			match = append(match, x)
		}
	}
	return match
}
