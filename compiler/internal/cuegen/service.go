package cuegen

import (
	"fmt"
	"reflect"

	"cuelang.org/go/cue/ast"
	"cuelang.org/go/cue/parser"
	"cuelang.org/go/cue/token"
	"golang.org/x/exp/slices"

	"encr.dev/parser/encoding"
	"encr.dev/parser/est"
	schema "encr.dev/proto/encore/parser/schema/v1"
)

// service represents the single generated file we will create for a service
// from all of it's config.Load calls
type service struct {
	g              *Generator
	svc            *est.Service
	file           *ast.File
	neededImports  map[string]string // map of package path to name
	topLevelFields []ast.Decl
	fieldLookup    map[string]*ast.Field

	typeUsage *definitionGenerator
}

// countNamedUsagesAndCollectImports counts the number of times a named type is used in the service
func (s *service) countNamedUsagesAndCollectImports(typ *schema.Type) error {
	return schema.Walk(s.g.res.Meta.Decls, typ, func(node any) error {
		switch typ := node.(type) {
		case *schema.Named:
			s.typeUsage.Inc(typ)

		case schema.Builtin:
			if typ == schema.Builtin_TIME {
				s.neededImports["time"] = "time"
			}
		}
		return nil
	})
}

func (s *service) registerTopLevelField(typ *schema.Type) error {
	concrete, err := encoding.GetConcreteStructType(s.g.res.Meta, typ, nil)
	if err != nil {
		return err
	}

	fields, err := s.structToFields(concrete)
	if err != nil {
		return err
	}

	for _, field := range fields {
		name, _, err := ast.LabelName(field.Label)
		if err != nil {
			return err
		}

		// If we already know about the field, merge the definitions
		// (this could be the case from multiple calls to `config.Load` inside
		// the same service to either the same or different struct types
		// with both have the same field name
		if existing, found := s.fieldLookup[name]; found {
			if len(field.Comments()) > 0 {
				if len(existing.Comments()) == 0 {
					existing.SetComments(field.Comments())
				} else {
					existingCommentGrp := existing.Comments()[0]
					for _, comment := range field.Comments() {
						if !commentAlreadyPresent(existing, comment) {
							existingCommentGrp.List = append(existingCommentGrp.List, comment.List...)
						}
					}

					// If after this check the existing comment group is now multiline, we need to move the comment groups
					// position to be before the field label.
					if len(existingCommentGrp.List) > 0 {
						existingCommentGrp.Position = 0
						existingCommentGrp.List[0].Slash = token.NewSection.Pos()
					}
				}
			}

			// Merge the values if they are different
			if !reflect.DeepEqual(existing.Value, field.Value) {
				existing.Value = ast.NewBinExpr(token.AND, existing.Value, field.Value)
			}
		} else {
			// otherwise add this field
			s.fieldLookup[name] = field
			s.topLevelFields = append(s.topLevelFields, field)
		}
	}

	return nil
}

func (s *service) generateCue() error {
	// If there are no top level fields, we've got nothing to do here
	if len(s.topLevelFields) == 0 {
		return nil
	}

	// Add the package name and decription comment
	pkg := &ast.Package{Name: ast.NewIdent(s.svc.Name)}
	s.file.Decls = append(s.file.Decls, pkg)
	s.file.AddComment(&ast.CommentGroup{
		List: []*ast.Comment{
			{Text: "// Code generated by encore. DO NOT EDIT."},
			{Text: "//"},
			{Text: "// The contents of this file are generated from the structs used in"},
			{Text: "// conjunction with Encore's `config.Load[T]()` function. This file"},
			{Text: "// automatically be regenerated if the data types within the struct"},
			{Text: "// are changed."},
			{Text: "//"},
			{Text: "// For more information about this file, see:"},
			{Text: "// https://encore.dev/docs/develop/config"},
		},
	})

	// Add any missing imports
	if len(s.neededImports) > 0 {
		// Get an ordered list of the imports
		imports := make([]string, 0, len(s.neededImports))
		for pkg := range s.neededImports {
			imports = append(imports, pkg)
		}
		slices.Sort(imports)

		// Create all the import specs
		for _, importPath := range imports {
			var ident *ast.Ident = nil
			if s.neededImports[importPath] != importPath {
				ident = ast.NewIdent(s.neededImports[importPath])
			}

			spec := ast.NewImport(ident, importPath)
			s.file.Imports = append(s.file.Imports, spec)
		}

		// Now add the import statement
		s.file.Decls = append(s.file.Decls, &ast.ImportDecl{
			Specs: s.file.Imports,
		})
	}

	// Write any declarations we've used multiple times to the file
	for _, named := range s.typeUsage.NamesWithCountsOver(1) {
		namedType := &schema.Type{Typ: &schema.Type_Named{Named: named}}
		decl := s.g.res.Meta.Decls[named.Id]
		concrete, err := encoding.GetConcreteType(s.g.res.Meta, namedType, nil)
		if err != nil {
			return err
		}

		defIdent := s.typeUsage.CueIdent(named)
		fieldType, err := s.toCueType(concrete)
		if err != nil {
			return err
		}

		field := &ast.Field{
			Label: defIdent,
			Value: fieldType,
		}
		if decl.Doc != "" {
			addCommentToField(field, decl.Doc)
		} else {
			// If there isn't a doc, we want to force a new section
			// above the name (empty line above).
			// The doc block will add this for us
			defIdent.NamePos = token.NewSection.Pos()
		}
		s.file.Decls = append(s.file.Decls, field)
	}

	// Now write the top level fields required in the config
	s.file.Decls = append(s.file.Decls, s.topLevelFields...)

	return nil
}

// structToFields converts a struct to a list of fields which can then be
// either included in a definition, a line struct or the file level declarations
func (s *service) structToFields(stru *schema.Struct) ([]*ast.Field, error) {
	var fields []*ast.Field

	for _, f := range stru.Fields {
		isOptional := false

		// Convert the type to CUE
		fieldType, err := s.toCueType(f.Typ)
		if err != nil {
			return nil, err
		}
		field := &ast.Field{
			Label: ast.NewIdent(f.Name),
			Value: fieldType,
		}

		for _, tag := range f.Tags {
			if tag.Key == "json" {
				if tag.Name != "" {
					field.Label = ast.NewIdent(tag.Name)
				}
				for _, option := range tag.Options {
					if option == "omitempty" {
						isOptional = true
					}
				}
			}

			if tag.Key == "cue" {
				if tag.Name != "" {
					expr, err := parser.ParseExpr("encore struct", tag.Name)
					if err != nil {
						return nil, err
					}
					field.Value = ast.NewBinExpr(token.AND, field.Value, expr)
				}
				for _, option := range tag.Options {
					if option == "opt" {
						isOptional = true
					}
				}
			}
		}

		// Mark the field as optional if it is
		if isOptional {
			field.Optional = token.Blank.Pos()
		}

		// Add the documentation to the field
		if f.Doc != "" {
			addCommentToField(field, f.Doc)
		}

		fields = append(fields, field)
	}

	return fields, nil
}

// Convert a schema type into a cue type
func (s *service) toCueType(unknownType *schema.Type) (ast.Expr, error) {
	switch typ := unknownType.Typ.(type) {
	case *schema.Type_Named:
		usageCount := s.typeUsage.Count(typ.Named)
		if usageCount <= 1 {
			// inline the type if it's only used once
			concrete, err := encoding.GetConcreteType(s.g.res.Meta, unknownType, nil)
			if err != nil {
				return nil, err
			}
			return s.toCueType(concrete)
		} else {
			return s.typeUsage.CueIdent(typ.Named), nil
		}
	case *schema.Type_Struct:
		fields, err := s.structToFields(typ.Struct)
		if err != nil {
			return nil, err
		}

		fieldsInterface := make([]interface{}, len(fields))
		for i, field := range fields {
			fieldsInterface[i] = field
		}

		return ast.NewStruct(fieldsInterface...), nil
	case *schema.Type_Map:
		keyType, err := s.toCueType(typ.Map.Key)
		if err != nil {
			return nil, err
		}
		valueType, err := s.toCueType(typ.Map.Value)
		if err != nil {
			return nil, err
		}
		return ast.NewStruct(ast.NewList(keyType), valueType), nil
	case *schema.Type_List:
		listType, err := s.toCueType(typ.List.Elem)
		if err != nil {
			return nil, err
		}
		return ast.NewList(&ast.Ellipsis{Type: listType}), nil
	case *schema.Type_Builtin:
		return s.builtinToCue(typ.Builtin), nil
	case *schema.Type_Config:
		// The config.Value type is a simple wrapper another type
		// and from the point of the CUE files, the wrapper is invisible
		return s.toCueType(typ.Config.Elem)
	default:
		panic(fmt.Sprintf("unexpected type: %T", typ))
	}
}

func (s *service) builtinToCue(builtin schema.Builtin) ast.Expr {
	switch builtin {
	case schema.Builtin_ANY:
		return ast.NewIdent("_") // top
	case schema.Builtin_BOOL:
		return ast.NewIdent("bool")
	case schema.Builtin_INT8:
		return ast.NewIdent("int8")
	case schema.Builtin_INT16:
		return ast.NewIdent("int16")
	case schema.Builtin_INT32:
		return ast.NewIdent("int32")
	case schema.Builtin_INT64:
		return ast.NewIdent("int64")
	case schema.Builtin_UINT8:
		return ast.NewIdent("uint8")
	case schema.Builtin_UINT16:
		return ast.NewIdent("uint16")
	case schema.Builtin_UINT32:
		return ast.NewIdent("uint32")
	case schema.Builtin_UINT64:
		return ast.NewIdent("uint64")
	case schema.Builtin_FLOAT32:
		return ast.NewIdent("float32")
	case schema.Builtin_FLOAT64:
		return ast.NewIdent("float64")
	case schema.Builtin_STRING:
		return ast.NewIdent("string")
	case schema.Builtin_BYTES:
		return ast.NewIdent("bytes")
	case schema.Builtin_TIME:
		return ast.NewSel(ast.NewIdent("time"), "Time")
	case schema.Builtin_UUID:
		return ast.NewIdent("string")
	case schema.Builtin_JSON:
		return ast.NewIdent("string")
	case schema.Builtin_USER_ID:
		return ast.NewIdent("string")
	case schema.Builtin_INT:
		return ast.NewIdent("int")
	case schema.Builtin_UINT:
		return ast.NewIdent("uint")
	default:
		panic(fmt.Sprintf("unknown builtin: %s", builtin))
	}
}
