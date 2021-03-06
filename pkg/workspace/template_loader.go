package workspace

import (
	"fmt"
	"strings"

	"github.com/k14s/ytt/pkg/files"
	"github.com/k14s/ytt/pkg/template"
	"github.com/k14s/ytt/pkg/template/core"
	"github.com/k14s/ytt/pkg/texttemplate"
	"github.com/k14s/ytt/pkg/yamlmeta"
	"github.com/k14s/ytt/pkg/yamltemplate"
	"github.com/k14s/ytt/pkg/yttlibrary"
	"go.starlark.net/starlark"
)

type TemplateLoader struct {
	ui                 files.UI
	values             *yamlmeta.Document
	opts               TemplateLoaderOpts
	compiledTemplates  map[string]*template.CompiledTemplate
	libraryExecFactory *LibraryExecutionFactory
}

type TemplateLoaderOpts struct {
	IgnoreUnknownComments bool
	StrictYAML            bool
}

func NewTemplateLoader(values *yamlmeta.Document, ui files.UI, opts TemplateLoaderOpts,
	libraryExecFactory *LibraryExecutionFactory) *TemplateLoader {

	if values == nil {
		panic("Expected values to be non-nil")
	}

	return &TemplateLoader{
		ui:                 ui,
		values:             values,
		opts:               opts,
		compiledTemplates:  map[string]*template.CompiledTemplate{},
		libraryExecFactory: libraryExecFactory,
	}
}

func (l *TemplateLoader) FindCompiledTemplate(path string) (*template.CompiledTemplate, error) {
	ct, found := l.compiledTemplates[path]
	if !found {
		return nil, fmt.Errorf("Expected to find '%s' compiled template", path)
	}
	return ct, nil
}

func (l *TemplateLoader) Load(thread *starlark.Thread, module string) (starlark.StringDict, error) {
	if api, found := l.getYTTLibrary(thread)[module]; found {
		return api, nil
	}

	libraryCtx := LibraryExecutionContext{
		Current: l.getCurrentLibrary(thread),
		Root:    l.getRootLibrary(thread),
	}
	filePath := module

	if strings.HasPrefix(module, "@") {
		pieces := strings.SplitN(module[1:], ":", 2)
		if len(pieces) != 2 {
			return nil, fmt.Errorf("Expected library path to be in format '@name:path', " +
				" for example, '@github.com/k14s/test:test.star'")
		}

		foundLib, err := libraryCtx.Current.FindAccessibleLibrary(pieces[0])
		if err != nil {
			return nil, err
		}

		libraryCtx = LibraryExecutionContext{Current: foundLib, Root: foundLib}
		filePath = pieces[1]
	}

	var libraryWithFile *Library = libraryCtx.Current

	// If path starts from a root, then it should be relative to root library
	if files.IsRootPath(filePath) {
		libraryWithFile = libraryCtx.Root
		filePath = files.StripRootPath(filePath)
	}

	fileInLib, err := libraryWithFile.FindFile(filePath)
	if err != nil {
		return nil, err
	}

	// File might be inside nested libraries, make sure to update current library
	libraryCtx = LibraryExecutionContext{Current: fileInLib.Library, Root: libraryCtx.Root}
	file := fileInLib.File

	if !file.IsLibrary() {
		return nil, fmt.Errorf("File '%s' is not a library file "+
			"(use data.read(...) for loading non-templated file contents into a variable)", file.RelativePath())
	}

	switch file.Type() {
	case files.TypeYAML:
		globals, _, err := l.EvalYAML(libraryCtx, file)
		return globals, err

	case files.TypeStarlark:
		return l.EvalStarlark(libraryCtx, file)

	case files.TypeText:
		globals, _, err := l.EvalText(libraryCtx, file)
		return globals, err

	default:
		return nil, fmt.Errorf("File '%s' type is not a known", file.RelativePath())
	}
}

func (l *TemplateLoader) ListData(thread *starlark.Thread, f *starlark.Builtin,
	args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {

	if args.Len() != 0 {
		return starlark.None, fmt.Errorf("expected exactly zero arguments")
	}

	result := []starlark.Value{}
	for _, fileInLib := range l.getCurrentLibrary(thread).ListAccessibleFiles() {
		result = append(result, starlark.String(fileInLib.File.RelativePath()))
	}
	return starlark.NewList(result), nil
}

func (l *TemplateLoader) LoadData(thread *starlark.Thread, f *starlark.Builtin,
	args starlark.Tuple, kwargs []starlark.Tuple) (starlark.Value, error) {

	if args.Len() != 1 {
		return starlark.None, fmt.Errorf("expected exactly one argument")
	}

	path, err := core.NewStarlarkValue(args.Index(0)).AsString()
	if err != nil {
		return starlark.None, err
	}

	fileInLib, err := l.getCurrentLibrary(thread).FindFile(path)
	if err != nil {
		return nil, err
	}

	fileBs, err := fileInLib.File.Bytes()
	if err != nil {
		return nil, err
	}

	return starlark.String(string(fileBs)), nil
}

func (l *TemplateLoader) ParseYAML(file *files.File) (*yamlmeta.DocumentSet, error) {
	fileBs, err := file.Bytes()
	if err != nil {
		return nil, err
	}

	docSetOpts := yamlmeta.DocSetOpts{
		AssociatedName: file.RelativePath(),
		WithoutMeta:    !file.IsTemplate() && !file.IsLibrary(),
		Strict:         l.opts.StrictYAML,
	}
	l.ui.Debugf("## file %s (opts %#v)\n", file.RelativePath(), docSetOpts)

	docSet, err := yamlmeta.NewDocumentSetFromBytes(fileBs, docSetOpts)
	if err != nil {
		return nil, fmt.Errorf("Unmarshaling YAML template '%s': %s", file.RelativePath(), err)
	}

	return docSet, nil
}

func (l *TemplateLoader) EvalYAML(libraryCtx LibraryExecutionContext, file *files.File) (starlark.StringDict, *yamlmeta.DocumentSet, error) {
	docSet, err := l.ParseYAML(file)
	if err != nil {
		return nil, nil, err
	}

	l.ui.Debugf("### ast\n")
	docSet.Print(l.ui.DebugWriter())

	if !file.IsTemplate() && !file.IsLibrary() {
		return nil, docSet, nil
	}

	tplOpts := yamltemplate.TemplateOpts{IgnoreUnknownComments: l.opts.IgnoreUnknownComments}

	compiledTemplate, err := yamltemplate.NewTemplate(file.RelativePath(), tplOpts).Compile(docSet)
	if err != nil {
		return nil, nil, fmt.Errorf("Compiling YAML template '%s': %s", file.RelativePath(), err)
	}

	l.addCompiledTemplate(file.RelativePath(), compiledTemplate)
	l.ui.Debugf("### template\n%s", compiledTemplate.DebugCodeAsString())

	yttLibrary := yttlibrary.NewAPI(compiledTemplate.TplReplaceNode,
		l.values, l, NewLibraryModule(libraryCtx, l.libraryExecFactory).AsModule())

	thread := l.newThread(libraryCtx, yttLibrary, file)

	globals, resultVal, err := compiledTemplate.Eval(thread, l)
	if err != nil {
		return nil, nil, err
	}

	return globals, resultVal.(*yamlmeta.DocumentSet), nil
}

func (l *TemplateLoader) EvalText(libraryCtx LibraryExecutionContext, file *files.File) (starlark.StringDict, *texttemplate.NodeRoot, error) {
	fileBs, err := file.Bytes()
	if err != nil {
		return nil, nil, err
	}

	l.ui.Debugf("## file %s\n", file.RelativePath())

	if !file.IsTemplate() && !file.IsLibrary() {
		plainRootNode := &texttemplate.NodeRoot{
			Items: []interface{}{&texttemplate.NodeText{Content: string(fileBs)}},
		}
		return nil, plainRootNode, nil
	}

	textRoot, err := texttemplate.NewParser().Parse(fileBs, file.RelativePath())
	if err != nil {
		return nil, nil, fmt.Errorf("Parsing text template '%s': %s", file.RelativePath(), err)
	}

	compiledTemplate, err := texttemplate.NewTemplate(file.RelativePath()).Compile(textRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("Compiling text template '%s': %s", file.RelativePath(), err)
	}

	l.addCompiledTemplate(file.RelativePath(), compiledTemplate)
	l.ui.Debugf("### template\n%s", compiledTemplate.DebugCodeAsString())

	yttLibrary := yttlibrary.NewAPI(compiledTemplate.TplReplaceNode,
		l.values, l, NewLibraryModule(libraryCtx, l.libraryExecFactory).AsModule())

	thread := l.newThread(libraryCtx, yttLibrary, file)

	globals, resultVal, err := compiledTemplate.Eval(thread, l)
	if err != nil {
		return nil, nil, fmt.Errorf("Evaluating text template: %s", err)
	}

	return globals, resultVal.(*texttemplate.NodeRoot), nil
}

func (l *TemplateLoader) EvalStarlark(libraryCtx LibraryExecutionContext, file *files.File) (starlark.StringDict, error) {
	fileBs, err := file.Bytes()
	if err != nil {
		return nil, err
	}

	l.ui.Debugf("## file %s\n", file.RelativePath())

	instructions := template.NewInstructionSet()
	compiledTemplate := template.NewCompiledTemplate(
		file.RelativePath(), template.NewCodeFromBytes(fileBs, instructions),
		instructions, template.NewNodes(), template.EvaluationCtxDialects{})

	l.addCompiledTemplate(file.RelativePath(), compiledTemplate)
	l.ui.Debugf("### template\n%s", compiledTemplate.DebugCodeAsString())

	yttLibrary := yttlibrary.NewAPI(compiledTemplate.TplReplaceNode,
		l.values, l, NewLibraryModule(libraryCtx, l.libraryExecFactory).AsModule())

	thread := l.newThread(libraryCtx, yttLibrary, file)

	globals, _, err := compiledTemplate.Eval(thread, l)
	if err != nil {
		return nil, fmt.Errorf("Evaluating starlark template: %s", err)
	}

	return globals, nil
}

const (
	threadCurrentLibraryKey = "ytt.curr_library_key"
	threadRootLibraryKey    = "ytt.root_library_key"
	threadYTTLibraryKey     = "ytt.ytt_library_key"
)

func (l *TemplateLoader) getCurrentLibrary(thread *starlark.Thread) *Library {
	lib, ok := thread.Local(threadCurrentLibraryKey).(*Library)
	if !ok || lib == nil {
		panic("Expected to find library associated with thread")
	}
	return lib
}

func (l *TemplateLoader) setCurrentLibrary(thread *starlark.Thread, library *Library) {
	thread.SetLocal(threadCurrentLibraryKey, library)
}

func (l *TemplateLoader) getRootLibrary(thread *starlark.Thread) *Library {
	lib, ok := thread.Local(threadRootLibraryKey).(*Library)
	if !ok || lib == nil {
		panic("Expected to find root library associated with thread")
	}
	return lib
}

func (l *TemplateLoader) setRootLibrary(thread *starlark.Thread, library *Library) {
	thread.SetLocal(threadRootLibraryKey, library)
}

func (l *TemplateLoader) getYTTLibrary(thread *starlark.Thread) yttlibrary.API {
	yttLibrary, ok := thread.Local(threadYTTLibraryKey).(yttlibrary.API)
	if !ok {
		panic("Expected to find YTT library associated with thread")
	}
	return yttLibrary
}

func (l *TemplateLoader) setYTTLibrary(thread *starlark.Thread, yttLibrary yttlibrary.API) {
	thread.SetLocal(threadYTTLibraryKey, yttLibrary)
}

func (l *TemplateLoader) newThread(libraryCtx LibraryExecutionContext,
	yttLibrary yttlibrary.API, file *files.File) *starlark.Thread {

	thread := &starlark.Thread{Name: "template=" + file.RelativePath(), Load: l.Load}
	l.setCurrentLibrary(thread, libraryCtx.Current)
	l.setRootLibrary(thread, libraryCtx.Root)
	l.setYTTLibrary(thread, yttLibrary)
	return thread
}

func (l *TemplateLoader) addCompiledTemplate(path string, ct *template.CompiledTemplate) {
	l.compiledTemplates[path] = ct
}
