package goja

import (
	"bufio"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/dop251/goja/parser"
	"github.com/dop251/goja/unistring"
)

type Debugger struct {
	vm *vm

	currentLine     int
	lastLine        int
	breakpoints     map[string][]int
	activationCh    chan chan ActivationReason
	currentCh       chan ActivationReason
	active          bool
	breakNext       bool
	breakNextReason ActivationReason
	lastBreakpoint  struct {
		filename   string
		line       int
		stackDepth int
	}
}

func newDebugger(vm *vm) *Debugger {
	dbg := &Debugger{
		vm:           vm,
		activationCh: make(chan chan ActivationReason),
		active:       false,
		breakpoints:  make(map[string][]int),
		lastLine:     0,
	}
	return dbg
}

type ActivationReason string

const (
	ProgramStartActivation      ActivationReason = "start"
	DebuggerStatementActivation ActivationReason = "debugger"
	BreakpointActivation        ActivationReason = "breakpoint"
)

var globalBuiltinKeys = map[string]struct{}{
	"Object":             {},
	"Function":           {},
	"Array":              {},
	"String":             {},
	"globalThis":         {},
	"NaN":                {},
	"undefined":          {},
	"Infinity":           {},
	"isNaN":              {},
	"parseInt":           {},
	"parseFloat":         {},
	"isFinite":           {},
	"decodeURI":          {},
	"decodeURIComponent": {},
	"encodeURI":          {},
	"encodeURIComponent": {},
	"escape":             {},
	"unescape":           {},
	"Number":             {},
	"RegExp":             {},
	"Date":               {},
	"Boolean":            {},
	"Proxy":              {},
	"Reflect":            {},
	"Error":              {},
	"AggregateError":     {},
	"TypeError":          {},
	"ReferenceError":     {},
	"SyntaxError":        {},
	"RangeError":         {},
	"EvalError":          {},
	"URIError":           {},
	"GoError":            {},
	"eval":               {},
	"Math":               {},
	"JSON":               {},
	"ArrayBuffer":        {},
	"DataView":           {},
	"Uint8Array":         {},
	"Uint8ClampedArray":  {},
	"Int8Array":          {},
	"Uint16Array":        {},
	"Int16Array":         {},
	"Uint32Array":        {},
	"Int32Array":         {},
	"Float32Array":       {},
	"Float64Array":       {},
	"Symbol":             {},
	"WeakSet":            {},
	"WeakMap":            {},
	"Map":                {},
	"Set":                {},
	"Promise":            {},
}

func (dbg *Debugger) activate(reason ActivationReason) {
	dbg.breakNext = false
	if dbg.breakNextReason != "" {
		reason = dbg.breakNextReason
		dbg.breakNextReason = ""
	}
	dbg.active = true
	ch := <-dbg.activationCh // get channel from waiter
	ch <- reason             // send what activated it
	<-ch                     // wait for deactivation
	dbg.active = false
}

// Continue unblocks the goja runtime to run code as is and will return the reason why it blocked again.
func (dbg *Debugger) Continue() ActivationReason {
	if dbg.currentCh != nil {
		close(dbg.currentCh)
	}
	dbg.currentCh = make(chan ActivationReason)
	dbg.activationCh <- dbg.currentCh
	reason := <-dbg.currentCh
	return reason
}

func (dbg *Debugger) PC() int {
	return dbg.vm.pc
}

// Detach the debugger, after this call this instance of the debugger should *not* be used.
// This also disables debug mode for the runtime
func (dbg *Debugger) Detach() { // TODO return an error?
	dbg.vm.debugger = nil
	dbg.vm = nil
	dbg.active = false
	if dbg.currentCh != nil {
		close(dbg.currentCh)
		dbg.currentCh = nil
	}
}

func (dbg *Debugger) SetBreakpoint(filename string, line int) (err error) {
	idx := sort.SearchInts(dbg.breakpoints[filename], line)
	if idx < len(dbg.breakpoints[filename]) && dbg.breakpoints[filename][idx] == line {
		err = errors.New("breakpoint exists")
	} else {
		dbg.breakpoints[filename] = append(dbg.breakpoints[filename], line)
		if len(dbg.breakpoints[filename]) > 1 {
			sort.Ints(dbg.breakpoints[filename])
		}
	}
	return
}

func (dbg *Debugger) ClearBreakpoint(filename string, line int) (err error) {
	if len(dbg.breakpoints[filename]) == 0 {
		return errors.New("no breakpoints")
	}

	idx := sort.SearchInts(dbg.breakpoints[filename], line)
	if idx < len(dbg.breakpoints[filename]) && dbg.breakpoints[filename][idx] == line {
		dbg.breakpoints[filename] = append(dbg.breakpoints[filename][:idx], dbg.breakpoints[filename][idx+1:]...)
		if len(dbg.breakpoints[filename]) == 0 {
			delete(dbg.breakpoints, filename)
		}
	} else {
		err = errors.New("breakpoint doesn't exist")
	}
	return
}

func (dbg *Debugger) Breakpoints() (map[string][]int, error) {
	if len(dbg.breakpoints) == 0 {
		return nil, errors.New("no breakpoints")
	}

	return dbg.breakpoints, nil
}

// returns true on instructions that can call vm.run() or vm.debug() themselves, or if we're at the
// end of the current code stack
func (dbg *Debugger) shouldRunAsync() bool {
	if dbg.vm.pc >= len(dbg.vm.prg.code)-1 {
		return true
	}
	switch dbg.vm.prg.code[dbg.vm.pc].(type) {
	case
		call, _callVariadic,
		callEval, _callEvalVariadic,
		callEvalStrict, _callEvalVariadicStrict,
		superCall, _superCallVariadic,
		*yieldMarker:
		return true
	}
	return false
}

func (dbg *Debugger) runOneStepAsync() error {
	dbg.breakNext = true
	dbg.lastBreakpoint.filename = ""
	dbg.lastBreakpoint.line = -1
	dbg.lastBreakpoint.stackDepth = -1
	reason := dbg.Continue()
	if reason != BreakpointActivation {
		return fmt.Errorf("unexpected breakpoint reason stepping into builtin function (%v)", reason)
	}
	return nil
}

func (dbg *Debugger) StepIn() error {
	// TODO: implement proper error propagation
	lastLine := dbg.Line()
	dbg.updateCurrentLine()
	if dbg.getLastLine() != dbg.Line() {
		currLine := dbg.Line()
		currFile := dbg.Filename()
		for dbg.safeToRun() && dbg.Line() == currLine && dbg.Filename() == currFile {
			dbg.updateCurrentLine()
			if dbg.shouldRunAsync() {
				err := dbg.runOneStepAsync()
				if err != nil {
					return err
				}
			} else {
				dbg.vm.prg.code[dbg.vm.pc].exec(dbg.vm)
			}
		}
		dbg.updateLastLine(lastLine)
	} else if dbg.getNextLine() == 0 {
		// Step out of functions
		for dbg.safeToRun() && dbg.vm.pc < len(dbg.vm.prg.code)-1 {
			dbg.vm.prg.code[dbg.vm.pc].exec(dbg.vm)
		}
		return dbg.runOneStepAsync()
	} else if dbg.vm.halted() {
		// Step out of program
		return errors.New("halted")
	}
	return nil
}

func (dbg *Debugger) StepOut() error {
	// TODO: implement proper error propagation
	lastLine := dbg.Line()
	dbg.updateCurrentLine()
	for dbg.safeToRun() {
		if dbg.vm.pc >= len(dbg.vm.prg.code)-1 {
			err := dbg.runOneStepAsync()
			if err != nil {
				return err
			}
			dbg.updateLastLine(lastLine)
			break
		} else {
			dbg.vm.prg.code[dbg.vm.pc].exec(dbg.vm)
		}
		dbg.updateLastLine(lastLine)
	}
	if dbg.vm.halted() {
		return errors.New("halted")
	}
	return nil
}

func (dbg *Debugger) StepPC() error {
	// TODO: implement proper error propagation
	lastLine := dbg.Line()
	dbg.updateCurrentLine()
	if dbg.safeToRun() {
		dbg.updateCurrentLine()
		if dbg.shouldRunAsync() {
			err := dbg.runOneStepAsync()
			if err != nil {
				return err
			}
		} else {
			dbg.vm.prg.code[dbg.vm.pc].exec(dbg.vm)
		}
		dbg.updateLastLine(lastLine)
	} else if dbg.vm.halted() {
		return errors.New("halted")
	}
	return nil
}

func (dbg *Debugger) Next() error {
	// TODO: implement proper error propagation
	lastLine := dbg.Line()
	nextLine := dbg.getNextLine()
	dbg.updateCurrentLine()
	if dbg.getLastLine() != dbg.Line() && nextLine != 0 {
		currFile := dbg.Filename()
		for dbg.safeToRun() && (dbg.Line() != nextLine || dbg.Filename() != currFile) {
			dbg.updateCurrentLine()
			if dbg.shouldRunAsync() {
				err := dbg.runOneStepAsync()
				if err != nil {
					return err
				}
			} else {
				dbg.vm.prg.code[dbg.vm.pc].exec(dbg.vm)
			}
		}
		dbg.updateLastLine(lastLine)
	} else if nextLine == 0 {
		// Step out of functions
		for dbg.safeToRun() && dbg.vm.pc < len(dbg.vm.prg.code)-1 {
			dbg.vm.prg.code[dbg.vm.pc].exec(dbg.vm)
		}
		return dbg.runOneStepAsync()
	} else if dbg.vm.halted() {
		// Step out of program
		return errors.New("halted")
	}
	return nil
}

func (dbg *Debugger) Exec(expr string) (Value, error) {
	if expr == "" {
		return nil, errors.New("nothing to execute")
	}
	val, err := dbg.eval(expr)

	lastLine := dbg.Line()
	dbg.updateLastLine(lastLine)
	return val, err
}

func (dbg *Debugger) Print(varName string) (string, error) {
	if varName == "" {
		return "", errors.New("please specify variable name")
	}
	if varName == "this" {
		varName = thisBindingName
	}
	val, err := dbg.getValue(varName)

	// String() panics on unresolved variables
	switch unresolved := val.(type) {
	case valueUnresolved:
		return fmt.Sprintf("variable '%s' is unresolvable", unresolved.ref.String()), err
	case memberUnresolved:
		return fmt.Sprintf("object has no member '%s'", unresolved.ref.String()), err
	}

	if val == Undefined() {
		return fmt.Sprint(dbg.vm.prg.values), err
	} else if val == nil {
		return fmt.Sprintf("error resolving variable '%s'", varName), err
	} else {
		return fmt.Sprint(val), err
	}
}

func (dbg *Debugger) List() ([]string, error) {
	// TODO probably better to get only some of the lines, but fine for now
	return stringToLines(dbg.vm.prg.src.Source())
}

func stringToLines(s string) (lines []string, err error) {
	scanner := bufio.NewScanner(strings.NewReader(s))
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	err = scanner.Err()
	return
}

func (dbg *Debugger) breakpoint() bool {
	if dbg.breakNext {
		// breakNext will be cleared in activate()
		return true
	}

	filename := dbg.Filename()
	line := dbg.Line()

	idx := sort.SearchInts(dbg.breakpoints[filename], line)
	if idx < len(dbg.breakpoints[filename]) && dbg.breakpoints[filename][idx] == line {
		return true
	} else {
		return false
	}
}

func (dbg *Debugger) getLastLine() int {
	if dbg.lastLine >= 0 {
		return dbg.lastLine
	}
	// First executed line (current line) is considered the last line
	return dbg.Line()
}

func (dbg *Debugger) updateLastLine(lineNumber int) {
	if dbg.lastLine != lineNumber {
		dbg.lastLine = lineNumber
	}
}

func (dbg *Debugger) callStackDepth() int {
	return len(dbg.vm.callStack)
}

func (dbg *Debugger) Line() int {
	// FIXME: Some lines are skipped, which causes this function to report incorrect lines
	// TODO: lines inside function are reported differently and the vm.pc is reset from the start
	// of each function, so account for functions (ref: TestDebuggerStepIn)
	return dbg.vm.prg.src.Position(dbg.vm.prg.sourceOffset(dbg.vm.pc)).Line
}

func (dbg *Debugger) Filename() string {
	return dbg.vm.prg.src.Name()
}

func (dbg *Debugger) updateCurrentLine() {
	dbg.currentLine = dbg.Line()
}

func (dbg *Debugger) getNextLine() int {
	for idx := range dbg.vm.prg.code[dbg.vm.pc:] {
		nextLine := dbg.vm.prg.src.Position(dbg.vm.prg.sourceOffset(dbg.vm.pc + idx + 1)).Line
		if nextLine > dbg.Line() {
			return nextLine
		}
	}
	return 0
}

func (dbg *Debugger) safeToRun() bool {
	return dbg.vm.pc < len(dbg.vm.prg.code)
}

func (dbg *Debugger) eval(expr string) (v Value, err error) {
	prg, err := parser.ParseFile(nil, "<eval>", expr, 0)
	if err != nil {
		return nil, &CompilerSyntaxError{
			CompilerError: CompilerError{
				Message: err.Error(),
			},
		}
	}

	c := newCompiler(true)

	defer func() {
		if x := recover(); x != nil {
			c.p = nil
			switch ex := x.(type) {
			case *CompilerSyntaxError:
				err = ex
			default:
				err = fmt.Errorf("cannot recover from exception %s", ex)
			}
		}
	}()

	var this Value
	var inGlobal bool
	if dbg.vm.sb >= 0 {
		this = dbg.vm.stack[dbg.vm.sb]
		inGlobal = false
	} else {
		this = dbg.vm.r.globalObject
		inGlobal = true
	}

	c.compile(prg, false, inGlobal, dbg.vm)

	defer func() {
		if x := recover(); x != nil {
			if ex := asUncatchableException(x); ex != nil {
				err = ex
			} else {
				err = fmt.Errorf("cannot recover from exception %s", x)
			}
		}
		dbg.vm.popCtx()
		// dbg.vm.halt = false
		dbg.vm.sp -= 1
	}()

	dbg.vm.pushCtx()
	dbg.vm.prg = c.p
	dbg.vm.pc = 0
	dbg.vm.args = 0
	dbg.vm.result = _undefined
	dbg.vm.sb = dbg.vm.sp
	dbg.vm.push(this)
	dbg.vm.run()
	v = dbg.vm.result
	return v, err
}

func (dbg *Debugger) getValue(varName string) (val Value, err error) {
	defer func() {
		if err := recover(); err != nil {
			return
		}
	}()

	// copied from loadDynamicRef
	name := unistring.String(varName)
	for stash := dbg.vm.stash; stash != nil; stash = stash.outer {
		if v, exists := stash.getByName(name); exists {
			val = v
			break
		}
	}
	if val == nil {
		val = dbg.vm.r.globalObject.self.getStr(name, nil)
		if val == nil {
			val = valueUnresolved{r: dbg.vm.r, ref: name}
		}
	}
	return val, nil
}

func (dbg *Debugger) GetGlobalVariables() (map[string]Value, error) {
	defer func() {
		if err := recover(); err != nil {
			return
		}
	}()

	globals := make(map[string]Value)
	for _, name := range dbg.vm.r.globalObject.self.stringKeys(true, nil) {
		nameStr := name.String()
		if _, ok := globalBuiltinKeys[nameStr]; ok {
			continue
		}
		val := dbg.vm.r.globalObject.self.getStr(unistring.String(nameStr), nil)
		if val != nil {
			globals[nameStr] = val
		}
	}
	return globals, nil
}

func (dbg *Debugger) GetLocalVariables() (map[string]Value, error) {
	defer func() {
		if err := recover(); err != nil {
			return
		}
	}()

	locals := make(map[string]Value)
	for name := range dbg.vm.stash.names {
		val, _ := dbg.getValue(name.String())
		if val == nil {
			locals[name.String()] = Undefined()
		}
		locals[name.String()] = val
	}
	return locals, nil
}
