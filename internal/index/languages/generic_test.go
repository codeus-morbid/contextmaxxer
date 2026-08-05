package languages

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/codeus-morbid/contextmaxxer/internal/index"
	"github.com/codeus-morbid/contextmaxxer/internal/store"
)

// parseWith parses source with the real grammar registry so these tests
// exercise the same node kinds production sees.
func parseWith(t *testing.T, lang, source string) []store.Symbol {
	t.Helper()
	p, err := index.NewTreeSitterParser()
	require.NoError(t, err)
	defer p.Close()

	tree, err := p.Parse(context.Background(), []byte(source), lang)
	require.NoError(t, err)
	require.NotNil(t, tree)

	ext, ok := New(lang)
	require.True(t, ok, "no extractor for %s", lang)
	return ext.Symbols(tree, []byte(source))
}

func symMap(syms []store.Symbol) map[string]store.Symbol {
	m := make(map[string]store.Symbol, len(syms))
	for _, s := range syms {
		m[s.QualifiedName] = s
	}
	return m
}

func TestGenericJava(t *testing.T) {
	syms := symMap(parseWith(t, "java", `
/** Greets people. */
public class Greeter {
    public Greeter() {}
    /** Says hello. */
    public String greet(String name) { return "hi " + name; }
}
interface Speaker { String speak(); }
enum Mode { ON, OFF }
`))
	require.Equal(t, store.KindClass, syms["Greeter"].Kind)
	require.Equal(t, store.KindMethod, syms["Greeter.greet"].Kind)
	require.Contains(t, syms["Greeter.greet"].Docstring, "Says hello")
	require.Equal(t, store.KindMethod, syms["Greeter.Greeter"].Kind)
	require.Equal(t, store.KindInterface, syms["Speaker"].Kind)
	require.Equal(t, store.KindMethod, syms["Speaker.speak"].Kind)
	require.Equal(t, store.KindType, syms["Mode"].Kind)
}

func TestGenericRust(t *testing.T) {
	syms := symMap(parseWith(t, "rust", `
/// A point.
pub struct Point { x: f64, y: f64 }

impl Point {
    /// Euclidean norm.
    pub fn norm(&self) -> f64 { (self.x * self.x + self.y * self.y).sqrt() }
}

impl Clone for Point {
    fn clone(&self) -> Self { *self }
}

pub trait Shape { fn area(&self) -> f64; }

pub fn free_fn() {}
`))
	require.Equal(t, store.KindType, syms["Point"].Kind)
	require.Contains(t, syms["Point"].Docstring, "A point")
	require.Equal(t, store.KindMethod, syms["Point.norm"].Kind)
	require.Equal(t, store.KindInterface, syms["Shape"].Kind)
	require.Equal(t, store.KindMethod, syms["Shape.area"].Kind)
	require.Equal(t, store.KindFunction, syms["free_fn"].Kind)
	// impl Clone for Point: the method belongs to the implementing TYPE
	// (impl_item's "type" field), not the trait.
	require.Equal(t, store.KindMethod, syms["Point.clone"].Kind, "impl Clone for Point: got %v", keys(syms))
}

func TestGenericC(t *testing.T) {
	syms := symMap(parseWith(t, "c", `
/* Adds two ints. */
int add(int a, int b) { return a + b; }

struct point { int x; int y; };

/* forward reference must NOT become a symbol */
struct point *make_point(void);

int *alloc_ints(int n) { return 0; }
`))
	require.Equal(t, store.KindFunction, syms["add"].Kind)
	require.Contains(t, syms["add"].Docstring, "Adds two ints")
	require.Equal(t, store.KindType, syms["point"].Kind)
	require.Equal(t, store.KindFunction, syms["alloc_ints"].Kind, "pointer declarator must resolve: %v", keys(syms))
	require.NotContains(t, syms, "make_point", "prototype (no body) must not be a function symbol")
}

func TestGenericCpp(t *testing.T) {
	syms := symMap(parseWith(t, "cpp", `
namespace geo {
class Circle {
public:
    double radius;
    double area() const { return 3.14159 * radius * radius; }
};

double Circle_area_out_of_line();
}

// Out-of-class definition.
double geo::Circle_area_out_of_line() { return 0; }
`))
	require.Equal(t, store.KindClass, syms["geo.Circle"].Kind)
	require.Equal(t, store.KindMethod, syms["geo.Circle.area"].Kind)
	// A ::-qualified top-level definition keeps its qualifier in the qname;
	// kind stays "function" (we can't cheaply tell ns::fn from Class::method).
	require.Equal(t, store.KindFunction, syms["geo.Circle_area_out_of_line"].Kind, "qualified out-of-line def: %v", keys(syms))
}

func TestGenericCSharp(t *testing.T) {
	syms := symMap(parseWith(t, "csharp", `
namespace App {
    /// <summary>Repo.</summary>
    public class UserRepo {
        public UserRepo() {}
        public string Find(int id) { return ""; }
    }
    public interface IClock { }
    public struct Vec2 { }
}
`))
	require.Equal(t, store.KindClass, syms["UserRepo"].Kind)
	require.Equal(t, store.KindMethod, syms["UserRepo.Find"].Kind)
	require.Equal(t, store.KindInterface, syms["IClock"].Kind)
	require.Equal(t, store.KindType, syms["Vec2"].Kind)
}

func TestGenericRuby(t *testing.T) {
	syms := symMap(parseWith(t, "ruby", `
# A greeter.
class Greeter
  # Says hello.
  def greet(name)
    "hi #{name}"
  end

  def self.default
    new
  end
end

module Util
  def self.clamp(x)
    x
  end
end

def free_helper
end
`))
	require.Equal(t, store.KindClass, syms["Greeter"].Kind)
	require.Contains(t, syms["Greeter"].Docstring, "A greeter")
	require.Equal(t, store.KindMethod, syms["Greeter.greet"].Kind)
	require.Equal(t, store.KindMethod, syms["Greeter.default"].Kind)
	require.Equal(t, store.KindType, syms["Util"].Kind)
	require.Equal(t, store.KindMethod, syms["Util.clamp"].Kind)
	require.Equal(t, store.KindFunction, syms["free_helper"].Kind)
}

func TestGenericPHP(t *testing.T) {
	syms := symMap(parseWith(t, "php", `<?php
/** Greets. */
class Greeter {
    public function greet(string $name): string { return "hi $name"; }
}
interface Speaker { public function speak(): string; }
function free_fn() {}
`))
	require.Equal(t, store.KindClass, syms["Greeter"].Kind)
	require.Equal(t, store.KindMethod, syms["Greeter.greet"].Kind)
	require.Equal(t, store.KindInterface, syms["Speaker"].Kind)
	require.Equal(t, store.KindFunction, syms["free_fn"].Kind)
}

func TestGenericKotlin(t *testing.T) {
	syms := symMap(parseWith(t, "kotlin", `
class Greeter(private val name: String) {
    fun greet(): String = "hi " + name
}
object Registry {
    fun lookup(id: Int): String = ""
}
fun freeHelper() {}
`))
	require.Equal(t, store.KindClass, syms["Greeter"].Kind, "got: %v", keys(syms))
	require.Equal(t, store.KindMethod, syms["Greeter.greet"].Kind)
	require.Equal(t, store.KindClass, syms["Registry"].Kind)
	require.Equal(t, store.KindMethod, syms["Registry.lookup"].Kind)
	require.Equal(t, store.KindFunction, syms["freeHelper"].Kind)
}

// Swift is deliberately absent: the alex-pinkus grammar does not commit the
// generated parser.c, so its Go module cannot build; revisit with a vendored
// parser if beta demand appears.

func TestGenericScala(t *testing.T) {
	syms := symMap(parseWith(t, "scala", `
class Greeter(name: String) {
  def greet(): String = "hi " + name
}
object Registry {
  def lookup(id: Int): String = ""
}
trait Speaker {
  def speak(): String
}
`))
	require.Equal(t, store.KindClass, syms["Greeter"].Kind, "got: %v", keys(syms))
	require.Equal(t, store.KindMethod, syms["Greeter.greet"].Kind)
	require.Equal(t, store.KindClass, syms["Registry"].Kind)
	require.Equal(t, store.KindMethod, syms["Registry.lookup"].Kind)
	require.Equal(t, store.KindInterface, syms["Speaker"].Kind)
	require.Equal(t, store.KindMethod, syms["Speaker.speak"].Kind)
}

// edgesFor parses source and extracts edges against the given symbol table.
func edgesFor(t *testing.T, lang, source string, nameToID map[string]int64) []store.Edge {
	t.Helper()
	p, err := index.NewTreeSitterParser()
	require.NoError(t, err)
	defer p.Close()
	tree, err := p.Parse(context.Background(), []byte(source), lang)
	require.NoError(t, err)
	ext, ok := New(lang)
	require.True(t, ok)
	return ext.Edges(tree, []byte(source), nameToID)
}

func edgeSet(edges []store.Edge) map[[2]int64]bool {
	m := map[[2]int64]bool{}
	for _, e := range edges {
		m[[2]int64{e.Src, e.Dst}] = true
	}
	return m
}

func TestGenericEdgesJava(t *testing.T) {
	src := `
public class A {
    void b() { c(); helper.save(); new Widget().run(); }
    void c() {}
}
class Widget { void run() {} }
class Other { void save() {} }
class Dup1 { void ambiguous() {} }
class Dup2 { void ambiguous() {} }
`
	ids := map[string]int64{
		"A": 1, "A.b": 2, "A.c": 3,
		"Widget": 4, "Widget.Widget": 40, "Widget.run": 5,
		"Other": 6, "Other.save": 7,
		"Dup1.ambiguous": 8, "Dup2.ambiguous": 9,
	}
	got := edgeSet(edgesFor(t, "java", src, ids))
	require.True(t, got[[2]int64{2, 3}], "b -> c intra-class")
	require.True(t, got[[2]int64{2, 7}], "b -> Other.save via unique suffix")
	require.True(t, got[[2]int64{2, 4}], "new Widget() -> class symbol")
	require.False(t, got[[2]int64{2, 8}] || got[[2]int64{2, 9}],
		"ambiguous suffix must yield no edge (hairball guard)")
}

func TestGenericEdgesRust(t *testing.T) {
	src := `
pub struct Point;
impl Point {
    pub fn norm(&self) -> f64 { helpers::scale(1.0); self.len() }
    fn len(&self) -> f64 { 0.0 }
}
pub fn scale(x: f64) -> f64 { x }
`
	ids := map[string]int64{"Point": 1, "Point.norm": 2, "Point.len": 3, "scale": 4}
	got := edgeSet(edgesFor(t, "rust", src, ids))
	require.True(t, got[[2]int64{2, 4}], "helpers::scale -> scale (scoped)")
	require.True(t, got[[2]int64{2, 3}], "self.len() -> Point.len (field)")
}

func TestGenericEdgesC(t *testing.T) {
	src := `
static int helper(int x) { return x; }
int compute(int x) { return helper(x) + missing(x); }
`
	ids := map[string]int64{"helper": 1, "compute": 2}
	got := edgeSet(edgesFor(t, "c", src, ids))
	require.True(t, got[[2]int64{2, 1}], "compute -> helper")
	require.Len(t, got, 1, "unknown callee emits nothing")
}

func TestGenericEdgesCpp(t *testing.T) {
	src := `
namespace app {
class Engine {
public:
    void start() { warmup(); util::log(); }
    void warmup() {}
};
void log() {}
}
`
	ids := map[string]int64{
		"app.Engine": 1, "app.Engine.start": 2, "app.Engine.warmup": 3, "app.log": 4,
	}
	got := edgeSet(edgesFor(t, "cpp", src, ids))
	require.True(t, got[[2]int64{2, 3}], "start -> warmup")
	require.True(t, got[[2]int64{2, 4}], "util::log -> app.log via suffix")
}

func TestGenericEdgesCSharp(t *testing.T) {
	src := `
class Service {
    void Handle() { Validate(); repo.Save(); var w = new Worker(); }
    void Validate() {}
}
class Repo { public void Save() {} }
class Worker {}
`
	ids := map[string]int64{
		"Service": 1, "Service.Handle": 2, "Service.Validate": 3,
		"Repo": 4, "Repo.Save": 5, "Worker": 6,
	}
	got := edgeSet(edgesFor(t, "csharp", src, ids))
	require.True(t, got[[2]int64{2, 3}], "Handle -> Validate")
	require.True(t, got[[2]int64{2, 5}], "repo.Save() -> Repo.Save via suffix")
	require.True(t, got[[2]int64{2, 6}], "new Worker() -> class symbol")
}

func TestGenericEdgesPHP(t *testing.T) {
	src := `<?php
namespace App;
class Svc {
    public function run() { $this->check(); Helper::persist(); load(); $w = new Widget(); }
    private function check() {}
}
class Helper { public static function persist() {} }
class Widget {}
function load() {}
`
	ids := map[string]int64{
		"Svc": 1, "Svc.run": 2, "Svc.check": 3,
		"Helper": 4, "Helper.persist": 5, "Widget": 6, "load": 7,
	}
	got := edgeSet(edgesFor(t, "php", src, ids))
	require.True(t, got[[2]int64{2, 3}], "$this->check()")
	require.True(t, got[[2]int64{2, 5}], "Helper::persist() via suffix")
	require.True(t, got[[2]int64{2, 7}], "load() free function")
	require.True(t, got[[2]int64{2, 6}], "new Widget() -> class")
}

func TestGenericEdgesRuby(t *testing.T) {
	src := `class Svc
  def run
    helper.persist
    Widget.new
    load_all()
  end
  def check; end
end
class Widget; end
class Helper
  def persist; end
end
def load_all; end
`
	ids := map[string]int64{
		"Svc": 1, "Svc.run": 2, "Svc.check": 3,
		"Widget": 4, "Helper": 5, "Helper.persist": 6, "load_all": 7,
	}
	got := edgeSet(edgesFor(t, "ruby", src, ids))
	require.True(t, got[[2]int64{2, 6}], "helper.persist via suffix")
	require.True(t, got[[2]int64{2, 4}], "Widget.new -> class symbol")
	require.True(t, got[[2]int64{2, 7}], "load_all()")
}

func TestGenericEdgesKotlin(t *testing.T) {
	src := `class Svc {
    fun run() { check(); helper.persist(); Widget() }
    fun check() {}
}
class Widget
class Helper { fun persist() {} }
`
	ids := map[string]int64{
		"Svc": 1, "Svc.run": 2, "Svc.check": 3,
		"Widget": 4, "Helper": 5, "Helper.persist": 6,
	}
	got := edgeSet(edgesFor(t, "kotlin", src, ids))
	require.True(t, got[[2]int64{2, 3}], "check()")
	require.True(t, got[[2]int64{2, 6}], "helper.persist() via navigation")
	require.True(t, got[[2]int64{2, 4}], "Widget() constructor -> class")
}

func TestGenericEdgesScala(t *testing.T) {
	src := `class Svc {
  def run(): Unit = { check(); helper.persist(); new Widget() }
  def check(): Unit = {}
}
class Widget {}
class Helper { def persist(): Unit = {} }
`
	ids := map[string]int64{
		"Svc": 1, "Svc.run": 2, "Svc.check": 3,
		"Widget": 4, "Helper": 5, "Helper.persist": 6,
	}
	got := edgeSet(edgesFor(t, "scala", src, ids))
	require.True(t, got[[2]int64{2, 3}], "check()")
	require.True(t, got[[2]int64{2, 6}], "helper.persist via field_expression")
	require.True(t, got[[2]int64{2, 4}], "new Widget() -> class")
}

func TestGenericEdgesNilWithoutCallsConfig(t *testing.T) {
	source := []byte("public class A { void b() { c(); } void c() {} }")
	p, err := index.NewTreeSitterParser()
	require.NoError(t, err)
	defer p.Close()
	tree, err := p.Parse(context.Background(), source, "java")
	require.NoError(t, err)

	ext := &genericExtractor{cfg: genericConfig{
		containers: map[string]string{"class_declaration": store.KindClass},
		callables:  map[string]bool{"method_declaration": true},
	}}
	edges := ext.Edges(tree, source, map[string]int64{"A.b": 1, "A.c": 2})
	require.Empty(t, edges, "no calls config = symbols-only")
}

func keys(m map[string]store.Symbol) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// Doc comments must survive the attribute/annotation nodes that idiomatic
// code puts between the comment and the declaration. These siblings used to
// end the comment scan, which cost Rust most of its docs (fd: 369 `///`
// lines in source, 68 documented symbols indexed).
func TestDocCommentSurvivesAttributes(t *testing.T) {
	syms := symMap(parseWith(t, "rust", `
/// Walks the directory tree and yields entries.
#[inline]
pub fn walk_tree(root: &Path) -> Vec<Entry> {
    Vec::new()
}

/// Formats the entry for display.
#[allow(dead_code)]
#[must_use]
pub fn format_entry(e: &Entry) -> String {
    String::new()
}

/// Not attached: a blank line separates this from the item.

pub fn undocumented(x: i32) -> i32 { x }
`))

	require.Contains(t, syms["walk_tree"].Docstring, "Walks the directory tree",
		"an #[inline] attribute between doc and fn must not drop the doc")
	require.Contains(t, syms["format_entry"].Docstring, "Formats the entry",
		"multiple stacked attributes must not drop the doc")
}

// A blank line between a comment and a declaration means the comment does not
// document it. The check was off by one wherever the grammar's comment node
// swallows the trailing newline (rust line_comment ends at the next row,
// column 0), and Go had no check at all — so license headers and unrelated
// notes were indexed as documentation.
func TestBlankLineDetachesDocComment(t *testing.T) {
	rust := symMap(parseWith(t, "rust", `
/// Attached: sits directly above.
pub fn documented(x: i32) -> i32 { x }

/// Detached: a blank line follows.

pub fn undocumented(x: i32) -> i32 { x }
`))
	require.Contains(t, rust["documented"].Docstring, "Attached")
	require.Empty(t, rust["undocumented"].Docstring, "blank line must detach the comment")

	syms := parseWith(t, "go", `package p

// Copyright (c) 2026. All rights reserved.

// Sum adds two numbers.
func Sum(a, b int) int { return a + b }

// Unrelated note about the file.

func Bare(a int) int { return a }
`)
	goSyms := symMap(syms)
	require.Contains(t, goSyms["p.Sum"].Docstring, "Sum adds two numbers")
	require.NotContains(t, goSyms["p.Sum"].Docstring, "Copyright",
		"a license header separated by a blank line is not the symbol's doc")
	require.Empty(t, goSyms["p.Bare"].Docstring)
}
