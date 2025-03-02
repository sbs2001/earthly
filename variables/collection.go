package variables

import (
	"fmt"
	"strings"

	"github.com/earthly/earthly/conslogging"
	"github.com/earthly/earthly/domain"
	"github.com/earthly/earthly/util/gitutil"

	dfShell "github.com/moby/buildkit/frontend/dockerfile/shell"
	specs "github.com/opencontainers/image-spec/specs-go/v1"
)

type stackFrame struct {
	frameName string
	// absRef is the ref any other ref in this frame would be relative to.
	absRef  domain.Reference
	imports *domain.ImportTracker

	// Always inactive scopes. These scopes only influence newly declared
	// args. They do not otherwise participate when args are expanded.
	overriding *Scope

	// Always active scopes. These scopes influence the value of args directly.
	args    *Scope
	globals *Scope
}

// Collection is a collection of variable scopes used within a single target.
type Collection struct {
	// These scopes are always present, regardless of the stack position.
	builtin *Scope // inactive
	envs    *Scope // active

	stack []*stackFrame

	// A scope containing all scopes above, combined.
	effectiveCache *Scope

	console conslogging.ConsoleLogger
}

// NewCollection creates a new Collection to be used in the context of a target.
func NewCollection(console conslogging.ConsoleLogger, target domain.Target, platform specs.Platform, gitMeta *gitutil.GitMetadata, overridingVars *Scope, globalImports map[string]domain.ImportTrackerVal) *Collection {
	return &Collection{
		builtin: BuiltinArgs(target, platform, gitMeta),
		envs:    NewScope(),
		stack: []*stackFrame{{
			frameName:  target.StringCanonical(),
			absRef:     target,
			imports:    domain.NewImportTracker(console, globalImports),
			overriding: overridingVars,
			args:       NewScope(),
			globals:    NewScope(),
		}},
		console: console,
	}
}

// ResetEnvVars resets the collection's env vars.
func (c *Collection) ResetEnvVars(envs *Scope) {
	if envs == nil {
		envs = NewScope()
	}
	c.envs = envs
	c.effectiveCache = nil
}

// EnvVars returns a copy of the env vars.
func (c *Collection) EnvVars() *Scope {
	return c.envs.Clone()
}

// Globals returns a copy of the globals.
func (c *Collection) Globals() *Scope {
	return c.globals().Clone()
}

// SetGlobals sets the global variables.
func (c *Collection) SetGlobals(globals *Scope) {
	c.frame().globals = globals
	c.effectiveCache = nil
}

// Overriding returns a copy of the overriding args.
func (c *Collection) Overriding() *Scope {
	return c.overriding().Clone()
}

// SetOverriding sets the overriding args.
func (c *Collection) SetOverriding(overriding *Scope) {
	c.frame().overriding = overriding
	c.effectiveCache = nil
}

// SetPlatform sets the platform, updating the builtin args.
func (c *Collection) SetPlatform(platform specs.Platform) {
	SetPlatformArgs(c.builtin, platform)
	c.effectiveCache = nil
}

// GetActive returns an active variable by name.
func (c *Collection) GetActive(name string) (string, bool) {
	return c.effective().GetActive(name)
}

// SortedActiveVariables returns the active variable names in a sorted slice.
func (c *Collection) SortedActiveVariables() []string {
	return c.effective().SortedActive()
}

// SortedOverridingVariables returns the overriding variable names in a sorted slice.
func (c *Collection) SortedOverridingVariables() []string {
	return c.overriding().SortedAny()
}

// Expand expands variables within the given word.
func (c *Collection) Expand(word string) string {
	shlex := dfShell.NewLex('\\')
	varMap := c.effective().ActiveValueMap()
	ret, err := shlex.ProcessWordWithMap(word, varMap)
	if err != nil {
		// No effect if there is an error.
		return word
	}
	return ret
}

// DeclareArg declares an arg. The effective value may be
// different than the default, if the variable has been overridden.
func (c *Collection) DeclareArg(name string, defaultValue string, global bool, pncvf ProcessNonConstantVariableFunc) (string, error) {
	ef := c.effective()
	var finalValue string
	existing, found := ef.GetAny(name)
	if found {
		finalValue = existing
	} else {
		v, err := parseArgValue(name, defaultValue, pncvf)
		if err != nil {
			return "", err
		}
		finalValue = v
	}
	c.args().AddActive(name, finalValue)
	if global {
		c.globals().AddActive(name, finalValue)
	}
	c.effectiveCache = nil
	return finalValue, nil
}

// SetArg sets the value of an arg.
func (c *Collection) SetArg(name string, value string) {
	c.args().AddActive(name, value)
	c.effectiveCache = nil
}

// UnsetArg removes an arg if it exists.
func (c *Collection) UnsetArg(name string) {
	c.args().Remove(name)
	c.effectiveCache = nil
}

// DeclareEnv declares an env var.
func (c *Collection) DeclareEnv(name string, value string) {
	c.envs.AddActive(name, value)
	c.effectiveCache = nil
}

// Imports returns the imports tracker of the current frame.
func (c *Collection) Imports() *domain.ImportTracker {
	return c.frame().imports
}

// EnterFrame creates a new stack frame.
func (c *Collection) EnterFrame(frameName string, absRef domain.Reference, overriding *Scope, globals *Scope, globalImports map[string]domain.ImportTrackerVal) {
	c.stack = append(c.stack, &stackFrame{
		frameName:  frameName,
		absRef:     absRef,
		imports:    domain.NewImportTracker(c.console, globalImports),
		overriding: overriding,
		globals:    globals,
		args:       NewScope(),
	})
	c.effectiveCache = nil
}

// ExitFrame exits the latest stack frame.
func (c *Collection) ExitFrame() {
	if len(c.stack) == 0 {
		panic("trying to pop an empty argsStack")
	}
	c.stack = c.stack[:(len(c.stack) - 1)]
	c.effectiveCache = nil
}

// AbsRef returns a ref that any other reference should be relative to as part of the stack frame.
func (c *Collection) AbsRef() domain.Reference {
	return c.frame().absRef
}

// IsStackAtBase returns whether the stack has size 1.
func (c *Collection) IsStackAtBase() bool {
	return len(c.stack) == 1
}

// StackString returns the stack as a string.
func (c *Collection) StackString() string {
	builder := make([]string, 0, len(c.stack))
	for i := len(c.stack) - 1; i >= 0; i-- {
		overridingNames := c.stack[i].overriding.SortedAny()
		row := make([]string, 0, len(overridingNames)+1)
		row = append(row, c.stack[i].frameName)
		for _, k := range overridingNames {
			v, _ := c.stack[i].overriding.GetAny(k)
			row = append(row, fmt.Sprintf("--%s=%s", k, v))
		}
		builder = append(builder, strings.Join(row, " "))
	}
	return strings.Join(builder, "\ncalled from\t")
}

func (c *Collection) frame() *stackFrame {
	return c.stack[len(c.stack)-1]
}

func (c *Collection) args() *Scope {
	return c.frame().args
}

func (c *Collection) globals() *Scope {
	return c.frame().globals
}

func (c *Collection) overriding() *Scope {
	return c.frame().overriding
}

// effective returns the variables as a single combined scope.
func (c *Collection) effective() *Scope {
	if c.effectiveCache == nil {
		c.effectiveCache = CombineScopes(c.overriding(), c.builtin, c.args(), c.envs, c.globals())
	}
	return c.effectiveCache
}
