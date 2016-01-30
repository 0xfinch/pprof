// Copyright 2014 Google Inc. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package binutils

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/google/pprof/internal/plugin"
)

// TestFindSymbols tests the FindSymbols routine by using a fake nm
// script.
func TestFindSymbols(t *testing.T) {
	type testcase struct {
		query string
		want  []plugin.Sym
	}

	testcases := []testcase{
		{
			"line.*[AC]",
			[]plugin.Sym{
				{[]string{"lineA001"}, "object.o", 0x1000, 0x1FFF},
				{[]string{"line200A"}, "object.o", 0x2000, 0x2FFF},
				{[]string{"lineB00C"}, "object.o", 0x3000, 0x3FFF},
			},
		},
		{
			"Dumb::operator",
			[]plugin.Sym{
				{[]string{"Dumb::operator()(char const*) const"}, "object.o", 0x3000, 0x3FFF},
			},
		},
	}

	const nm = "testdata/wrapper/nm"
	for _, tc := range testcases {
		syms, err := findSymbols(nm, "object.o", regexp.MustCompile(tc.query), 0)
		if err != nil {
			t.Fatalf("%q: findSymbols: %v", tc.query, err)
		}
		if err := checkSymbol(syms, tc.want); err != nil {
			t.Errorf("%q: %v", tc.query, err)
		}
	}
}

func checkSymbol(got []*plugin.Sym, want []plugin.Sym) error {
	if len(got) != len(want) {
		return fmt.Errorf("unexpected number of symbols %d (want %d)\n", len(got), len(want))
	}

	for i, g := range got {
		w := want[i]
		if len(g.Name) != len(w.Name) {
			return fmt.Errorf("names, got %d, want %d", len(g.Name), len(w.Name))
		}
		for n := range g.Name {
			if g.Name[n] != w.Name[n] {
				return fmt.Errorf("name %d, got %q, want %q", n, g.Name[n], w.Name[n])
			}
		}
		if g.File != w.File {
			return fmt.Errorf("filename, got %q, want %q", g.File, w.File)
		}
		if g.Start != w.Start {
			return fmt.Errorf("start address, got %#x, want %#x", g.Start, w.Start)
		}
		if g.End != w.End {
			return fmt.Errorf("end address, got %#x, want %#x", g.End, w.End)
		}
	}
	return nil
}

// TestFunctionAssembly tests the FunctionAssembly routine by using a
// fake objdump script.
func TestFunctionAssembly(t *testing.T) {
	type testcase struct {
		s    plugin.Sym
		want []plugin.Inst
	}
	testcases := []testcase{
		{
			plugin.Sym{[]string{"symbol1"}, "", 0x1000, 0x1FFF},
			[]plugin.Inst{
				{0x1000, "instruction one", "", 0},
				{0x1001, "instruction two", "", 0},
				{0x1002, "instruction three", "", 0},
				{0x1003, "instruction four", "", 0},
			},
		},
		{
			plugin.Sym{[]string{"symbol2"}, "", 0x2000, 0x2FFF},
			[]plugin.Inst{
				{0x2000, "instruction one", "", 0},
				{0x2001, "instruction two", "", 0},
			},
		},
	}

	const objdump = "testdata/wrapper/objdump"

	for _, tc := range testcases {
		insns, err := disassemble(objdump, "object.o", tc.s.Start, tc.s.End)
		if err != nil {
			t.Fatalf("FunctionAssembly: %v", err)
		}

		if len(insns) != len(tc.want) {
			t.Errorf("Unexpected number of assembly instructions %d (want %d)\n", len(insns), len(tc.want))
		}
		for i := range insns {
			if insns[i] != tc.want[i] {
				t.Errorf("Expected symbol %v, got %v\n", tc.want[i], insns[i])
			}
		}
	}
}
