package asp

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strconv"
	"strings"

	"cli"
	"core"
)

const (
	// ANSI formatting codes
	reset     = "\033[0m"
	boldRed   = "\033[31;1m"
	boldWhite = "\033[37;1m"
	red       = "\033[31m"
	yellow    = "\033[33m"
	white     = "\033[37m"
	grey      = "\033[30m"
)

// errDeferParse indicates that a package needs to wait until another target is built.
type errDeferParse struct {
	Label core.BuildLabel // The target we're waiting for
}

func (err errDeferParse) Error() string {
	return "Requires build of " + err.Label.String()
}

// RequiresSubinclude returns true if the error requires another target to be built, along with the target in question.
func RequiresSubinclude(err error) (bool, core.BuildLabel) {
	if dp, ok := err.(errDeferParse); ok {
		return true, dp.Label
	}
	return false, core.BuildLabel{}
}

// An errorStack is an error that carries an internal stack trace.
type errorStack struct {
	// From top down, i.e. Stack[0] is the innermost function in the call stack.
	Stack []Position
	// Readers that correspond to each level in the stack trace.
	// Each may be nil but this will always have the same length as Stack.
	Readers []io.ReadSeeker
	// The original error that was encountered.
	err error
}

// fail panics on lex/parse errors in a file.
// For convenience we reuse errorStack although there is of course not really a call stack at this point.
func fail(pos Position, message string, args ...interface{}) {
	panic(AddStackFrame(pos, fmt.Errorf(message, args...)))
}

// AddStackFrame adds a new stack frame to the given errorStack, or wraps an existing error if not.
func AddStackFrame(pos Position, err interface{}) error {
	stack, ok := err.(*errorStack)
	if !ok {
		if dp, ok := err.(errDeferParse); ok {
			return dp // Does not need stack information
		} else if e, ok := err.(error); ok {
			stack = &errorStack{err: e}
		} else {
			stack = &errorStack{err: fmt.Errorf("%s", err)}
		}
	} else if n := len(stack.Stack) - 1; n > 0 && stack.Stack[n].Filename == pos.Filename && stack.Stack[n].Line == pos.Line {
		return stack // Don't duplicate the same line multiple times. Often happens since one line can have multiple expressions.
	}
	stack.Stack = append(stack.Stack, pos)
	stack.Readers = append(stack.Readers, nil)
	return stack
}

// AddReader adds an io.Reader to an errStack, which will allow it to recover more information from that file.
func AddReader(err error, r io.ReadSeeker) error {
	if stack, ok := err.(*errorStack); ok {
		stack.AddReader(r)
	}
	return err
}

// Error implements the builtin error interface.
func (stack *errorStack) Error() string {
	if len(stack.Stack) > 1 {
		return stack.errorMessage() + "\n" + stack.stackTrace()
	}
	return stack.errorMessage()
}

// ShortError returns an abbreviated message with jsut what immediately went wrong.
func (stack *errorStack) ShortError() string {
	return stack.err.Error()
}

// stackTrace returns the lines of stacktrace from the error.
func (stack *errorStack) stackTrace() string {
	ret := make([]string, len(stack.Stack))
	filenames := make([]string, len(stack.Stack))
	lines := make([]string, len(stack.Stack))
	cols := make([]string, len(stack.Stack))
	for i, frame := range stack.Stack {
		filenames[i] = frame.Filename
		lines[i] = strconv.Itoa(frame.Line)
		cols[i] = strconv.Itoa(frame.Column)
	}
	stack.equaliseLengths(filenames)
	stack.equaliseLengths(lines)
	stack.equaliseLengths(cols)
	// Add final message & colours if appropriate
	lastLine := 0
	lastFile := ""
	for i, frame := range stack.Stack {
		if frame.Line == lastLine && frame.Filename == lastFile {
			continue // Don't show the same line twice.
		}
		_, line, _ := stack.readLine(stack.Readers[i], frame.Line-1)
		if line == "" {
			line = "<source unavailable>"
			if cli.StdErrIsATerminal {
				line = grey + line + reset
			}
		}
		s := fmt.Sprintf("%s:%s:%s:", filenames[i], lines[i], cols[i])
		if !cli.StdErrIsATerminal {
			ret[i] = fmt.Sprintf("%s   %s", s, line)
		} else {
			ret[i] = fmt.Sprintf("%s%s%s   %s", yellow, s, reset, line)
		}
		lastLine = frame.Line
		lastFile = frame.Filename
	}
	msg := "Traceback:\n"
	if cli.StdErrIsATerminal {
		msg = boldWhite + msg + reset
	}
	return msg + strings.Join(ret, "\n")
}

// equaliseLengths left-pads the given strings so they are all of equal length.
func (stack *errorStack) equaliseLengths(sl []string) {
	max := 0
	for _, s := range sl {
		if len(s) > max {
			max = len(s)
		}
	}
	for i, s := range sl {
		sl[i] = strings.Repeat(" ", max-len(s)) + s
	}
}

// errorMessage returns the first part of the error message (i.e. the main message & file context)
func (stack *errorStack) errorMessage() string {
	// Take the outermost call in the stack since that is usually the most relevant to people.
	n := len(stack.Stack) - 1
	frame := stack.Stack[n]
	if before, line, after := stack.readLine(stack.Readers[n], frame.Line-1); line != "" || before != "" || after != "" {
		charsBefore := frame.Column - 1
		if charsBefore < 0 { // strings.Repeat panics if negative
			charsBefore = 0
		} else if charsBefore == len(line) {
			line = line + "  "
		} else if charsBefore > len(line) {
			return stack.Error() // probably something's gone wrong and we're on totally the wrong line.
		}
		spaces := strings.Repeat(" ", charsBefore)
		if !cli.StdErrIsATerminal {
			return fmt.Sprintf("%s:%d:%d: error: %s\n%s\n%s\n%s^\n%s\n",
				frame.Filename, frame.Line, frame.Column, stack.err, before, line, spaces, after)
		}
		// Add colour hints as well. It's a bit weird to add them here where we don't know
		// how this is going to be printed, but not obvious how to solve well.
		return fmt.Sprintf("%s%s%s:%s%d%s:%s%d%s: %serror:%s %s%s%s\n%s%s\n%s%s%s%c%s%s\n%s^\n%s%s%s\n",
			boldWhite, frame.Filename, reset,
			boldWhite, frame.Line, reset,
			boldWhite, frame.Column, reset,
			boldRed, reset,
			boldWhite, stack.err, reset,
			grey, before,
			white, line[:charsBefore], red, line[charsBefore], white, line[charsBefore+1:],
			spaces,
			grey, after, reset,
		)
	}
	return stack.err.Error()
}

// readLine reads a particular line of a reader plus some context.
func (stack *errorStack) readLine(r io.ReadSeeker, line int) (string, string, string) {
	// The reader for any level of the stack is allowed to be nil.
	if r == nil {
		return "", "", ""
	}
	r.Seek(0, io.SeekStart)
	// This isn't 100% efficient but who cares really.
	b, err := ioutil.ReadAll(r)
	if err != nil {
		return "", "", ""
	}
	lines := bytes.Split(b, []byte{'\n'})
	if len(lines) <= line {
		return "", "", ""
	}
	before := ""
	if line > 0 {
		before = string(lines[line-1])
	}
	after := ""
	if line < len(lines)-1 {
		after = string(lines[line+1])
	}
	return before, string(lines[line]), after
}

// AddReader adds an io.Reader into this error where appropriate.
func (stack *errorStack) AddReader(r io.ReadSeeker) {
	for i, r2 := range stack.Readers {
		if r2 == nil {
			fn := stack.Stack[i].Filename
			if NameOfReader(r) == fn {
				stack.Readers[i] = r
			} else if f, err := os.Open(fn); err == nil {
				// Maybe it's just a file on disk (e.g. via subinclude)
				stack.Readers[i] = f
				// If it was generated by a filegroup, it might match the in-repo source.
				// In that case it's a little ugly to present the leading plz-out/gen.
				if fn2 := strings.TrimPrefix(fn, core.GenDir+"/"); fn2 != fn && core.IsSameFile(fn, fn2) {
					stack.Stack[i].Filename = fn2
				}
			}
		}
	}
}

// A namedReader implements Name() on a Reader, allowing the lexer to automatically retrieve its name.
// This is a bit awkward but unfortunately all we have when we try to access it is an io.Reader.
type namedReader struct {
	r    io.ReadSeeker
	name string
}

// Read implements the io.Reader interface
func (r *namedReader) Read(b []byte) (int, error) {
	return r.r.Read(b)
}

// Name implements the internal namer interface
func (r *namedReader) Name() string {
	return r.name
}

// Seek implements the io.Seeker interface
func (r *namedReader) Seek(offset int64, whence int) (int64, error) {
	return r.r.Seek(offset, whence)
}
