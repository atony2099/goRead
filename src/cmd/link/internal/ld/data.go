// Derived from Inferno utils/6l/obj.c and utils/6l/span.c
// https://bitbucket.org/inferno-os/inferno-os/src/default/utils/6l/obj.c
// https://bitbucket.org/inferno-os/inferno-os/src/default/utils/6l/span.c
//
//	Copyright © 1994-1999 Lucent Technologies Inc.  All rights reserved.
//	Portions Copyright © 1995-1997 C H Forsyth (forsyth@terzarima.net)
//	Portions Copyright © 1997-1999 Vita Nuova Limited
//	Portions Copyright © 2000-2007 Vita Nuova Holdings Limited (www.vitanuova.com)
//	Portions Copyright © 2004,2006 Bruce Ellis
//	Portions Copyright © 2005-2007 C H Forsyth (forsyth@terzarima.net)
//	Revisions Copyright © 2000-2007 Lucent Technologies Inc. and others
//	Portions Copyright © 2009 The Go Authors. All rights reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT.  IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package ld

import (
	"bytes"
	"cmd/internal/gcprog"
	"cmd/internal/objabi"
	"cmd/internal/sys"
	"cmd/link/internal/loader"
	"cmd/link/internal/sym"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// isRuntimeDepPkg reports whether pkg is the runtime package or its dependency
func isRuntimeDepPkg(pkg string) bool {
	switch pkg {
	case "runtime",
		"sync/atomic",      // runtime may call to sync/atomic, due to go:linkname
		"internal/bytealg", // for IndexByte
		"internal/cpu":     // for cpu features
		return true
	}
	return strings.HasPrefix(pkg, "runtime/internal/") && !strings.HasSuffix(pkg, "_test")
}

// Estimate the max size needed to hold any new trampolines created for this function. This
// is used to determine when the section can be split if it becomes too large, to ensure that
// the trampolines are in the same section as the function that uses them.
func maxSizeTrampolinesPPC64(ldr *loader.Loader, s loader.Sym, isTramp bool) uint64 {
	// If thearch.Trampoline is nil, then trampoline support is not available on this arch.
	// A trampoline does not need any dependent trampolines.
	if thearch.Trampoline == nil || isTramp {
		return 0
	}

	n := uint64(0)
	relocs := ldr.Relocs(s)
	for ri := 0; ri < relocs.Count(); ri++ {
		r := relocs.At2(ri)
		if r.Type().IsDirectCallOrJump() {
			n++
		}
	}
	// Trampolines in ppc64 are 4 instructions.
	return n * 16
}

// detect too-far jumps in function s, and add trampolines if necessary
// ARM, PPC64 & PPC64LE support trampoline insertion for internal and external linking
// On PPC64 & PPC64LE the text sections might be split but will still insert trampolines
// where necessary.
func trampoline(ctxt *Link, s loader.Sym) {
	if thearch.Trampoline == nil {
		return // no need or no support of trampolines on this arch
	}

	ldr := ctxt.loader
	relocs := ldr.Relocs(s)
	for ri := 0; ri < relocs.Count(); ri++ {
		r := relocs.At2(ri)
		if !r.Type().IsDirectCallOrJump() {
			continue
		}
		rs := r.Sym()
		if !ldr.AttrReachable(rs) || ldr.SymType(rs) == sym.Sxxx {
			continue // something is wrong. skip it here and we'll emit a better error later
		}
		rs = ldr.ResolveABIAlias(rs)
		if ldr.SymValue(rs) == 0 && (ldr.SymType(rs) != sym.SDYNIMPORT && ldr.SymType(rs) != sym.SUNDEFEXT) {
			if ldr.SymPkg(rs) != ldr.SymPkg(s) {
				if !isRuntimeDepPkg(ldr.SymPkg(s)) || !isRuntimeDepPkg(ldr.SymPkg(rs)) {
					ctxt.Errorf(s, "unresolved inter-package jump to %s(%s) from %s", ldr.SymName(rs), ldr.SymPkg(rs), ldr.SymPkg(s))
				}
				// runtime and its dependent packages may call to each other.
				// they are fine, as they will be laid down together.
			}
			continue
		}

		thearch.Trampoline(ctxt, ldr, ri, rs, s)
	}

}

// relocsym resolve relocations in "s". The main loop walks through
// the list of relocations attached to "s" and resolves them where
// applicable. Relocations are often architecture-specific, requiring
// calls into the 'archreloc' and/or 'archrelocvariant' functions for
// the architecture. When external linking is in effect, it may not be
// possible to completely resolve the address/offset for a symbol, in
// which case the goal is to lay the groundwork for turning a given
// relocation into an external reloc (to be applied by the external
// linker). For more on how relocations work in general, see
//
//  "Linkers and Loaders", by John R. Levine (Morgan Kaufmann, 1999), ch. 7
//
// This is a performance-critical function for the linker; be careful
// to avoid introducing unnecessary allocations in the main loop.
// TODO: This function is called in parallel. When the Loader wavefront
// reaches here, calls into the loader need to be parallel as well.
func relocsym(target *Target, ldr *loader.Loader, err *ErrorReporter, syms *ArchSyms, s *sym.Symbol) {
	if len(s.R) == 0 {
		return
	}
	if s.Attr.ReadOnly() {
		// The symbol's content is backed by read-only memory.
		// Copy it to writable memory to apply relocations.
		s.P = append([]byte(nil), s.P...)
		s.Attr.Set(sym.AttrReadOnly, false)
	}
	for ri := int32(0); ri < int32(len(s.R)); ri++ {
		r := &s.R[ri]
		if r.Done {
			// Relocation already processed by an earlier phase.
			continue
		}
		r.Done = true
		off := r.Off
		siz := int32(r.Siz)
		if off < 0 || off+siz > int32(len(s.P)) {
			rname := ""
			if r.Sym != nil {
				rname = r.Sym.Name
			}
			Errorf(s, "invalid relocation %s: %d+%d not in [%d,%d)", rname, off, siz, 0, len(s.P))
			continue
		}

		if r.Sym != nil && ((r.Sym.Type == sym.Sxxx && !r.Sym.Attr.VisibilityHidden()) || r.Sym.Type == sym.SXREF) {
			// When putting the runtime but not main into a shared library
			// these symbols are undefined and that's OK.
			if target.IsShared() || target.IsPlugin() {
				if r.Sym.Name == "main.main" || (!target.IsPlugin() && r.Sym.Name == "main..inittask") {
					r.Sym.Type = sym.SDYNIMPORT
				} else if strings.HasPrefix(r.Sym.Name, "go.info.") {
					// Skip go.info symbols. They are only needed to communicate
					// DWARF info between the compiler and linker.
					continue
				}
			} else {
				err.errorUnresolved(s, r)
				continue
			}
		}

		if r.Type >= objabi.ElfRelocOffset {
			continue
		}
		if r.Siz == 0 { // informational relocation - no work to do
			continue
		}

		// We need to be able to reference dynimport symbols when linking against
		// shared libraries, and Solaris, Darwin and AIX need it always
		if !target.IsSolaris() && !target.IsDarwin() && !target.IsAIX() && r.Sym != nil && r.Sym.Type == sym.SDYNIMPORT && !target.IsDynlinkingGo() && !r.Sym.Attr.SubSymbol() {
			if !(target.IsPPC64() && target.IsExternal() && r.Sym.Name == ".TOC.") {
				Errorf(s, "unhandled relocation for %s (type %d (%s) rtype %d (%s))", r.Sym.Name, r.Sym.Type, r.Sym.Type, r.Type, sym.RelocName(target.Arch, r.Type))
			}
		}
		if r.Sym != nil && r.Sym.Type != sym.STLSBSS && r.Type != objabi.R_WEAKADDROFF && !r.Sym.Attr.Reachable() {
			Errorf(s, "unreachable sym in relocation: %s", r.Sym.Name)
		}

		if target.IsExternal() {
			r.InitExt()
		}

		// TODO(mundaym): remove this special case - see issue 14218.
		if target.IsS390X() {
			switch r.Type {
			case objabi.R_PCRELDBL:
				r.InitExt()
				r.Type = objabi.R_PCREL
				r.Variant = sym.RV_390_DBL
			case objabi.R_CALL:
				r.InitExt()
				r.Variant = sym.RV_390_DBL
			}
		}

		var o int64
		switch r.Type {
		default:
			switch siz {
			default:
				Errorf(s, "bad reloc size %#x for %s", uint32(siz), r.Sym.Name)
			case 1:
				o = int64(s.P[off])
			case 2:
				o = int64(target.Arch.ByteOrder.Uint16(s.P[off:]))
			case 4:
				o = int64(target.Arch.ByteOrder.Uint32(s.P[off:]))
			case 8:
				o = int64(target.Arch.ByteOrder.Uint64(s.P[off:]))
			}
			if offset, ok := thearch.Archreloc(target, syms, r, s, o); ok {
				o = offset
			} else {
				Errorf(s, "unknown reloc to %v: %d (%s)", r.Sym.Name, r.Type, sym.RelocName(target.Arch, r.Type))
			}
		case objabi.R_TLS_LE:
			if target.IsExternal() && target.IsElf() {
				r.Done = false
				if r.Sym == nil {
					r.Sym = syms.Tlsg
				}
				r.Xsym = r.Sym
				r.Xadd = r.Add
				o = 0
				if !target.IsAMD64() {
					o = r.Add
				}
				break
			}

			if target.IsElf() && target.IsARM() {
				// On ELF ARM, the thread pointer is 8 bytes before
				// the start of the thread-local data block, so add 8
				// to the actual TLS offset (r->sym->value).
				// This 8 seems to be a fundamental constant of
				// ELF on ARM (or maybe Glibc on ARM); it is not
				// related to the fact that our own TLS storage happens
				// to take up 8 bytes.
				o = 8 + r.Sym.Value
			} else if target.IsElf() || target.IsPlan9() || target.IsDarwin() {
				o = int64(syms.Tlsoffset) + r.Add
			} else if target.IsWindows() {
				o = r.Add
			} else {
				log.Fatalf("unexpected R_TLS_LE relocation for %v", target.HeadType)
			}
		case objabi.R_TLS_IE:
			if target.IsExternal() && target.IsElf() {
				r.Done = false
				if r.Sym == nil {
					r.Sym = syms.Tlsg
				}
				r.Xsym = r.Sym
				r.Xadd = r.Add
				o = 0
				if !target.IsAMD64() {
					o = r.Add
				}
				break
			}
			if target.IsPIE() && target.IsElf() {
				// We are linking the final executable, so we
				// can optimize any TLS IE relocation to LE.
				if thearch.TLSIEtoLE == nil {
					log.Fatalf("internal linking of TLS IE not supported on %v", target.Arch.Family)
				}
				thearch.TLSIEtoLE(s, int(off), int(r.Siz))
				o = int64(syms.Tlsoffset)
				// TODO: o += r.Add when !target.IsAmd64()?
				// Why do we treat r.Add differently on AMD64?
				// Is the external linker using Xadd at all?
			} else {
				log.Fatalf("cannot handle R_TLS_IE (sym %s) when linking internally", s.Name)
			}
		case objabi.R_ADDR:
			if target.IsExternal() && r.Sym.Type != sym.SCONST {
				r.Done = false

				// set up addend for eventual relocation via outer symbol.
				rs := r.Sym

				r.Xadd = r.Add
				for rs.Outer != nil {
					r.Xadd += Symaddr(rs) - Symaddr(rs.Outer)
					rs = rs.Outer
				}

				if rs.Type != sym.SHOSTOBJ && rs.Type != sym.SDYNIMPORT && rs.Type != sym.SUNDEFEXT && rs.Sect == nil {
					Errorf(s, "missing section for relocation target %s", rs.Name)
				}
				r.Xsym = rs

				o = r.Xadd
				if target.IsElf() {
					if target.IsAMD64() {
						o = 0
					}
				} else if target.IsDarwin() {
					if rs.Type != sym.SHOSTOBJ {
						o += Symaddr(rs)
					}
				} else if target.IsWindows() {
					// nothing to do
				} else if target.IsAIX() {
					o = Symaddr(r.Sym) + r.Add
				} else {
					Errorf(s, "unhandled pcrel relocation to %s on %v", rs.Name, target.HeadType)
				}

				break
			}

			// On AIX, a second relocation must be done by the loader,
			// as section addresses can change once loaded.
			// The "default" symbol address is still needed by the loader so
			// the current relocation can't be skipped.
			if target.IsAIX() && r.Sym.Type != sym.SDYNIMPORT {
				// It's not possible to make a loader relocation in a
				// symbol which is not inside .data section.
				// FIXME: It should be forbidden to have R_ADDR from a
				// symbol which isn't in .data. However, as .text has the
				// same address once loaded, this is possible.
				if s.Sect.Seg == &Segdata {
					Xcoffadddynrel(target, ldr, s, r)
				}
			}

			o = Symaddr(r.Sym) + r.Add

			// On amd64, 4-byte offsets will be sign-extended, so it is impossible to
			// access more than 2GB of static data; fail at link time is better than
			// fail at runtime. See https://golang.org/issue/7980.
			// Instead of special casing only amd64, we treat this as an error on all
			// 64-bit architectures so as to be future-proof.
			if int32(o) < 0 && target.Arch.PtrSize > 4 && siz == 4 {
				Errorf(s, "non-pc-relative relocation address for %s is too big: %#x (%#x + %#x)", r.Sym.Name, uint64(o), Symaddr(r.Sym), r.Add)
				errorexit()
			}
		case objabi.R_DWARFSECREF:
			if r.Sym.Sect == nil {
				Errorf(s, "missing DWARF section for relocation target %s", r.Sym.Name)
			}

			if target.IsExternal() {
				r.Done = false

				// On most platforms, the external linker needs to adjust DWARF references
				// as it combines DWARF sections. However, on Darwin, dsymutil does the
				// DWARF linking, and it understands how to follow section offsets.
				// Leaving in the relocation records confuses it (see
				// https://golang.org/issue/22068) so drop them for Darwin.
				if target.IsDarwin() {
					r.Done = true
				}

				// PE code emits IMAGE_REL_I386_SECREL and IMAGE_REL_AMD64_SECREL
				// for R_DWARFSECREF relocations, while R_ADDR is replaced with
				// IMAGE_REL_I386_DIR32, IMAGE_REL_AMD64_ADDR64 and IMAGE_REL_AMD64_ADDR32.
				// Do not replace R_DWARFSECREF with R_ADDR for windows -
				// let PE code emit correct relocations.
				if !target.IsWindows() {
					r.Type = objabi.R_ADDR
				}

				r.Xsym = r.Sym.Sect.Sym
				r.Xadd = r.Add + Symaddr(r.Sym) - int64(r.Sym.Sect.Vaddr)

				o = r.Xadd
				if target.IsElf() && target.IsAMD64() {
					o = 0
				}
				break
			}
			o = Symaddr(r.Sym) + r.Add - int64(r.Sym.Sect.Vaddr)
		case objabi.R_WEAKADDROFF:
			if !r.Sym.Attr.Reachable() {
				continue
			}
			fallthrough
		case objabi.R_ADDROFF:
			// The method offset tables using this relocation expect the offset to be relative
			// to the start of the first text section, even if there are multiple.
			if r.Sym.Sect.Name == ".text" {
				o = Symaddr(r.Sym) - int64(Segtext.Sections[0].Vaddr) + r.Add
			} else {
				o = Symaddr(r.Sym) - int64(r.Sym.Sect.Vaddr) + r.Add
			}

		case objabi.R_ADDRCUOFF:
			// debug_range and debug_loc elements use this relocation type to get an
			// offset from the start of the compile unit.
			o = Symaddr(r.Sym) + r.Add - Symaddr(ldr.Syms[r.Sym.Unit.Textp2[0]])

			// r->sym can be null when CALL $(constant) is transformed from absolute PC to relative PC call.
		case objabi.R_GOTPCREL:
			if target.IsDynlinkingGo() && target.IsDarwin() && r.Sym != nil && r.Sym.Type != sym.SCONST {
				r.Done = false
				r.Xadd = r.Add
				r.Xadd -= int64(r.Siz) // relative to address after the relocated chunk
				r.Xsym = r.Sym

				o = r.Xadd
				o += int64(r.Siz)
				break
			}
			fallthrough
		case objabi.R_CALL, objabi.R_PCREL:
			if target.IsExternal() && r.Sym != nil && r.Sym.Type == sym.SUNDEFEXT {
				// pass through to the external linker.
				r.Done = false
				r.Xadd = 0
				if target.IsElf() {
					r.Xadd -= int64(r.Siz)
				}
				r.Xsym = r.Sym
				o = 0
				break
			}
			if target.IsExternal() && r.Sym != nil && r.Sym.Type != sym.SCONST && (r.Sym.Sect != s.Sect || r.Type == objabi.R_GOTPCREL) {
				r.Done = false

				// set up addend for eventual relocation via outer symbol.
				rs := r.Sym

				r.Xadd = r.Add
				for rs.Outer != nil {
					r.Xadd += Symaddr(rs) - Symaddr(rs.Outer)
					rs = rs.Outer
				}

				r.Xadd -= int64(r.Siz) // relative to address after the relocated chunk
				if rs.Type != sym.SHOSTOBJ && rs.Type != sym.SDYNIMPORT && rs.Sect == nil {
					Errorf(s, "missing section for relocation target %s", rs.Name)
				}
				r.Xsym = rs

				o = r.Xadd
				if target.IsElf() {
					if target.IsAMD64() {
						o = 0
					}
				} else if target.IsDarwin() {
					if r.Type == objabi.R_CALL {
						if target.IsExternal() && rs.Type == sym.SDYNIMPORT {
							if target.IsAMD64() {
								// AMD64 dynamic relocations are relative to the end of the relocation.
								o += int64(r.Siz)
							}
						} else {
							if rs.Type != sym.SHOSTOBJ {
								o += int64(uint64(Symaddr(rs)) - rs.Sect.Vaddr)
							}
							o -= int64(r.Off) // relative to section offset, not symbol
						}
					} else {
						o += int64(r.Siz)
					}
				} else if target.IsWindows() && target.IsAMD64() { // only amd64 needs PCREL
					// PE/COFF's PC32 relocation uses the address after the relocated
					// bytes as the base. Compensate by skewing the addend.
					o += int64(r.Siz)
				} else {
					Errorf(s, "unhandled pcrel relocation to %s on %v", rs.Name, target.HeadType)
				}

				break
			}

			o = 0
			if r.Sym != nil {
				o += Symaddr(r.Sym)
			}

			o += r.Add - (s.Value + int64(r.Off) + int64(r.Siz))
		case objabi.R_SIZE:
			o = r.Sym.Size + r.Add

		case objabi.R_XCOFFREF:
			if !target.IsAIX() {
				Errorf(s, "find XCOFF R_REF on non-XCOFF files")
			}
			if !target.IsExternal() {
				Errorf(s, "find XCOFF R_REF with internal linking")
			}
			r.Xsym = r.Sym
			r.Xadd = r.Add
			r.Done = false

			// This isn't a real relocation so it must not update
			// its offset value.
			continue

		case objabi.R_DWARFFILEREF:
			// The final file index is saved in r.Add in dwarf.go:writelines.
			o = r.Add
		}

		if target.IsPPC64() || target.IsS390X() {
			r.InitExt()
			if r.Variant != sym.RV_NONE {
				o = thearch.Archrelocvariant(target, syms, r, s, o)
			}
		}

		if false {
			nam := "<nil>"
			var addr int64
			if r.Sym != nil {
				nam = r.Sym.Name
				addr = Symaddr(r.Sym)
			}
			xnam := "<nil>"
			if r.Xsym != nil {
				xnam = r.Xsym.Name
			}
			fmt.Printf("relocate %s %#x (%#x+%#x, size %d) => %s %#x +%#x (xsym: %s +%#x) [type %d (%s)/%d, %x]\n", s.Name, s.Value+int64(off), s.Value, r.Off, r.Siz, nam, addr, r.Add, xnam, r.Xadd, r.Type, sym.RelocName(target.Arch, r.Type), r.Variant, o)
		}
		switch siz {
		default:
			Errorf(s, "bad reloc size %#x for %s", uint32(siz), r.Sym.Name)
			fallthrough

			// TODO(rsc): Remove.
		case 1:
			s.P[off] = byte(int8(o))
		case 2:
			if o != int64(int16(o)) {
				Errorf(s, "relocation address for %s is too big: %#x", r.Sym.Name, o)
			}
			i16 := int16(o)
			target.Arch.ByteOrder.PutUint16(s.P[off:], uint16(i16))
		case 4:
			if r.Type == objabi.R_PCREL || r.Type == objabi.R_CALL {
				if o != int64(int32(o)) {
					Errorf(s, "pc-relative relocation address for %s is too big: %#x", r.Sym.Name, o)
				}
			} else {
				if o != int64(int32(o)) && o != int64(uint32(o)) {
					Errorf(s, "non-pc-relative relocation address for %s is too big: %#x", r.Sym.Name, uint64(o))
				}
			}

			fl := int32(o)
			target.Arch.ByteOrder.PutUint32(s.P[off:], uint32(fl))
		case 8:
			target.Arch.ByteOrder.PutUint64(s.P[off:], uint64(o))
		}
	}
}

func (ctxt *Link) reloc() {
	var wg sync.WaitGroup
	target := &ctxt.Target
	ldr := ctxt.loader
	reporter := &ctxt.ErrorReporter
	syms := &ctxt.ArchSyms
	wg.Add(3)
	go func() {
		for _, s := range ctxt.Textp {
			relocsym(target, ldr, reporter, syms, s)
		}
		wg.Done()
	}()
	go func() {
		for _, s := range ctxt.datap {
			relocsym(target, ldr, reporter, syms, s)
		}
		wg.Done()
	}()
	go func() {
		for _, si := range dwarfp {
			for _, s := range si.syms {
				relocsym(target, ldr, reporter, syms, s)
			}
		}
		wg.Done()
	}()
	wg.Wait()
}

func windynrelocsym(ctxt *Link, rel *loader.SymbolBuilder, s loader.Sym) {
	var su *loader.SymbolBuilder
	relocs := ctxt.loader.Relocs(s)
	for ri := 0; ri < relocs.Count(); ri++ {
		r := relocs.At2(ri)
		targ := r.Sym()
		if targ == 0 {
			continue
		}
		rt := r.Type()
		if !ctxt.loader.AttrReachable(targ) {
			if rt == objabi.R_WEAKADDROFF {
				continue
			}
			ctxt.Errorf(s, "dynamic relocation to unreachable symbol %s",
				ctxt.loader.SymName(targ))
		}

		tplt := ctxt.loader.SymPlt(targ)
		tgot := ctxt.loader.SymGot(targ)
		if tplt == -2 && tgot != -2 { // make dynimport JMP table for PE object files.
			tplt := int32(rel.Size())
			ctxt.loader.SetPlt(targ, tplt)

			if su == nil {
				su = ctxt.loader.MakeSymbolUpdater(s)
			}
			r.SetSym(rel.Sym())
			r.SetAdd(int64(tplt))

			// jmp *addr
			switch ctxt.Arch.Family {
			default:
				ctxt.Errorf(s, "unsupported arch %v", ctxt.Arch.Family)
				return
			case sys.I386:
				rel.AddUint8(0xff)
				rel.AddUint8(0x25)
				rel.AddAddrPlus(ctxt.Arch, targ, 0)
				rel.AddUint8(0x90)
				rel.AddUint8(0x90)
			case sys.AMD64:
				rel.AddUint8(0xff)
				rel.AddUint8(0x24)
				rel.AddUint8(0x25)
				rel.AddAddrPlus4(ctxt.Arch, targ, 0)
				rel.AddUint8(0x90)
			}
		} else if tplt >= 0 {
			if su == nil {
				su = ctxt.loader.MakeSymbolUpdater(s)
			}
			r.SetSym(rel.Sym())
			r.SetAdd(int64(tplt))
		}
	}
}

// windynrelocsyms generates jump table to C library functions that will be
// added later. windynrelocsyms writes the table into .rel symbol.
func (ctxt *Link) windynrelocsyms() {
	if !(ctxt.IsWindows() && iscgo && ctxt.IsInternal()) {
		return
	}

	rel := ctxt.loader.LookupOrCreateSym(".rel", 0)
	relu := ctxt.loader.MakeSymbolUpdater(rel)
	relu.SetType(sym.STEXT)

	for _, s := range ctxt.Textp2 {
		windynrelocsym(ctxt, relu, s)
	}

	ctxt.Textp2 = append(ctxt.Textp2, rel)
}

func dynrelocsym(ctxt *Link, s *sym.Symbol) {
	target := &ctxt.Target
	ldr := ctxt.loader
	syms := &ctxt.ArchSyms
	for ri := range s.R {
		r := &s.R[ri]
		if ctxt.BuildMode == BuildModePIE && ctxt.LinkMode == LinkInternal {
			// It's expected that some relocations will be done
			// later by relocsym (R_TLS_LE, R_ADDROFF), so
			// don't worry if Adddynrel returns false.
			thearch.Adddynrel(target, ldr, syms, s, r)
			continue
		}

		if r.Sym != nil && r.Sym.Type == sym.SDYNIMPORT || r.Type >= objabi.ElfRelocOffset {
			if r.Sym != nil && !r.Sym.Attr.Reachable() {
				Errorf(s, "dynamic relocation to unreachable symbol %s", r.Sym.Name)
			}
			if !thearch.Adddynrel(target, ldr, syms, s, r) {
				Errorf(s, "unsupported dynamic relocation for symbol %s (type=%d (%s) stype=%d (%s))", r.Sym.Name, r.Type, sym.RelocName(ctxt.Arch, r.Type), r.Sym.Type, r.Sym.Type)
			}
		}
	}
}

func (state *dodataState) dynreloc(ctxt *Link) {
	if ctxt.HeadType == objabi.Hwindows {
		return
	}
	// -d suppresses dynamic loader format, so we may as well not
	// compute these sections or mark their symbols as reachable.
	if *FlagD {
		return
	}

	for _, s := range ctxt.Textp {
		dynrelocsym(ctxt, s)
	}
	for _, syms := range state.data {
		for _, s := range syms {
			dynrelocsym(ctxt, s)
		}
	}
	if ctxt.IsELF {
		elfdynhash(ctxt)
	}
}

func Codeblk(ctxt *Link, out *OutBuf, addr int64, size int64) {
	CodeblkPad(ctxt, out, addr, size, zeros[:])
}

func CodeblkPad(ctxt *Link, out *OutBuf, addr int64, size int64, pad []byte) {
	if *flagA {
		ctxt.Logf("codeblk [%#x,%#x) at offset %#x\n", addr, addr+size, ctxt.Out.Offset())
	}

	writeBlocks(out, ctxt.outSem, ctxt.Textp, addr, size, pad)

	/* again for printing */
	if !*flagA {
		return
	}

	syms := ctxt.Textp
	for i, s := range syms {
		if !s.Attr.Reachable() {
			continue
		}
		if s.Value >= addr {
			syms = syms[i:]
			break
		}
	}

	eaddr := addr + size
	for _, s := range syms {
		if !s.Attr.Reachable() {
			continue
		}
		if s.Value >= eaddr {
			break
		}

		if addr < s.Value {
			ctxt.Logf("%-20s %.8x|", "_", uint64(addr))
			for ; addr < s.Value; addr++ {
				ctxt.Logf(" %.2x", 0)
			}
			ctxt.Logf("\n")
		}

		ctxt.Logf("%.6x\t%-20s\n", uint64(addr), s.Name)
		q := s.P

		for len(q) >= 16 {
			ctxt.Logf("%.6x\t% x\n", uint64(addr), q[:16])
			addr += 16
			q = q[16:]
		}

		if len(q) > 0 {
			ctxt.Logf("%.6x\t% x\n", uint64(addr), q)
			addr += int64(len(q))
		}
	}

	if addr < eaddr {
		ctxt.Logf("%-20s %.8x|", "_", uint64(addr))
		for ; addr < eaddr; addr++ {
			ctxt.Logf(" %.2x", 0)
		}
	}
}

const blockSize = 1 << 20 // 1MB chunks written at a time.

// writeBlocks writes a specified chunk of symbols to the output buffer. It
// breaks the write up into ≥blockSize chunks to write them out, and schedules
// as many goroutines as necessary to accomplish this task. This call then
// blocks, waiting on the writes to complete. Note that we use the sem parameter
// to limit the number of concurrent writes taking place.
func writeBlocks(out *OutBuf, sem chan int, syms []*sym.Symbol, addr, size int64, pad []byte) {
	for i, s := range syms {
		if s.Value >= addr && !s.Attr.SubSymbol() {
			syms = syms[i:]
			break
		}
	}

	var wg sync.WaitGroup
	max, lastAddr, written := int64(blockSize), addr+size, int64(0)
	for addr < lastAddr {
		// Find the last symbol we'd write.
		idx := -1
		for i, s := range syms {
			if s.Attr.SubSymbol() {
				continue
			}

			// If the next symbol's size would put us out of bounds on the total length,
			// stop looking.
			if s.Value+s.Size > lastAddr {
				break
			}

			// We're gonna write this symbol.
			idx = i

			// If we cross over the max size, we've got enough symbols.
			if s.Value+s.Size > addr+max {
				break
			}
		}

		// If we didn't find any symbols to write, we're done here.
		if idx < 0 {
			break
		}

		// Compute the length to write, including padding.
		// We need to write to the end address (lastAddr), or the next symbol's
		// start address, whichever comes first. If there is no more symbols,
		// just write to lastAddr. This ensures we don't leave holes between the
		// blocks or at the end.
		length := int64(0)
		if idx+1 < len(syms) {
			// Find the next top-level symbol.
			// Skip over sub symbols so we won't split a containter symbol
			// into two blocks.
			next := syms[idx+1]
			for next.Attr.SubSymbol() {
				idx++
				next = syms[idx+1]
			}
			length = next.Value - addr
		}
		if length == 0 || length > lastAddr-addr {
			length = lastAddr - addr
		}

		// Start the block output operator.
		if o, err := out.View(uint64(out.Offset() + written)); err == nil {
			sem <- 1
			wg.Add(1)
			go func(o *OutBuf, syms []*sym.Symbol, addr, size int64, pad []byte) {
				writeBlock(o, syms, addr, size, pad)
				wg.Done()
				<-sem
			}(o, syms, addr, length, pad)
		} else { // output not mmaped, don't parallelize.
			writeBlock(out, syms, addr, length, pad)
		}

		// Prepare for the next loop.
		if idx != -1 {
			syms = syms[idx+1:]
		}
		written += length
		addr += length
	}
	wg.Wait()
}

func writeBlock(out *OutBuf, syms []*sym.Symbol, addr, size int64, pad []byte) {
	for i, s := range syms {
		if s.Value >= addr && !s.Attr.SubSymbol() {
			syms = syms[i:]
			break
		}
	}

	// This doesn't distinguish the memory size from the file
	// size, and it lays out the file based on Symbol.Value, which
	// is the virtual address. DWARF compression changes file sizes,
	// so dwarfcompress will fix this up later if necessary.
	eaddr := addr + size
	for _, s := range syms {
		if s.Attr.SubSymbol() {
			continue
		}
		if s.Value >= eaddr {
			break
		}
		if s.Value < addr {
			Errorf(s, "phase error: addr=%#x but sym=%#x type=%d", addr, s.Value, s.Type)
			errorexit()
		}
		if addr < s.Value {
			out.WriteStringPad("", int(s.Value-addr), pad)
			addr = s.Value
		}
		out.WriteSym(s)
		addr += int64(len(s.P))
		if addr < s.Value+s.Size {
			out.WriteStringPad("", int(s.Value+s.Size-addr), pad)
			addr = s.Value + s.Size
		}
		if addr != s.Value+s.Size {
			Errorf(s, "phase error: addr=%#x value+size=%#x", addr, s.Value+s.Size)
			errorexit()
		}
		if s.Value+s.Size >= eaddr {
			break
		}
	}

	if addr < eaddr {
		out.WriteStringPad("", int(eaddr-addr), pad)
	}
}

type writeFn func(*Link, *OutBuf, int64, int64)

// WriteParallel handles scheduling parallel execution of data write functions.
func WriteParallel(wg *sync.WaitGroup, fn writeFn, ctxt *Link, seek, vaddr, length uint64) {
	if out, err := ctxt.Out.View(seek); err != nil {
		ctxt.Out.SeekSet(int64(seek))
		fn(ctxt, ctxt.Out, int64(vaddr), int64(length))
	} else {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fn(ctxt, out, int64(vaddr), int64(length))
		}()
	}
}

func Datblk(ctxt *Link, out *OutBuf, addr, size int64) {
	writeDatblkToOutBuf(ctxt, out, addr, size)
}

// Used only on Wasm for now.
func DatblkBytes(ctxt *Link, addr int64, size int64) []byte {
	buf := make([]byte, size)
	out := &OutBuf{heap: buf}
	writeDatblkToOutBuf(ctxt, out, addr, size)
	return buf
}

func writeDatblkToOutBuf(ctxt *Link, out *OutBuf, addr int64, size int64) {
	if *flagA {
		ctxt.Logf("datblk [%#x,%#x) at offset %#x\n", addr, addr+size, ctxt.Out.Offset())
	}

	writeBlocks(out, ctxt.outSem, ctxt.datap, addr, size, zeros[:])

	/* again for printing */
	if !*flagA {
		return
	}

	syms := ctxt.datap
	for i, sym := range syms {
		if sym.Value >= addr {
			syms = syms[i:]
			break
		}
	}

	eaddr := addr + size
	for _, sym := range syms {
		if sym.Value >= eaddr {
			break
		}
		if addr < sym.Value {
			ctxt.Logf("\t%.8x| 00 ...\n", uint64(addr))
			addr = sym.Value
		}

		ctxt.Logf("%s\n\t%.8x|", sym.Name, uint64(addr))
		for i, b := range sym.P {
			if i > 0 && i%16 == 0 {
				ctxt.Logf("\n\t%.8x|", uint64(addr)+uint64(i))
			}
			ctxt.Logf(" %.2x", b)
		}

		addr += int64(len(sym.P))
		for ; addr < sym.Value+sym.Size; addr++ {
			ctxt.Logf(" %.2x", 0)
		}
		ctxt.Logf("\n")

		if ctxt.LinkMode != LinkExternal {
			continue
		}
		for i := range sym.R {
			r := &sym.R[i] // Copying sym.Reloc has measurable impact on performance
			rsname := ""
			rsval := int64(0)
			if r.Sym != nil {
				rsname = r.Sym.Name
				rsval = r.Sym.Value
			}
			typ := "?"
			switch r.Type {
			case objabi.R_ADDR:
				typ = "addr"
			case objabi.R_PCREL:
				typ = "pcrel"
			case objabi.R_CALL:
				typ = "call"
			}
			ctxt.Logf("\treloc %.8x/%d %s %s+%#x [%#x]\n", uint(sym.Value+int64(r.Off)), r.Siz, typ, rsname, r.Add, rsval+r.Add)
		}
	}

	if addr < eaddr {
		ctxt.Logf("\t%.8x| 00 ...\n", uint(addr))
	}
	ctxt.Logf("\t%.8x|\n", uint(eaddr))
}

func Dwarfblk(ctxt *Link, out *OutBuf, addr int64, size int64) {
	if *flagA {
		ctxt.Logf("dwarfblk [%#x,%#x) at offset %#x\n", addr, addr+size, ctxt.Out.Offset())
	}

	// Concatenate the section symbol lists into a single list to pass
	// to writeBlocks.
	//
	// NB: ideally we would do a separate writeBlocks call for each
	// section, but this would run the risk of undoing any file offset
	// adjustments made during layout.
	n := 0
	for i := range dwarfp {
		n += len(dwarfp[i].syms)
	}
	syms := make([]*sym.Symbol, 0, n)
	for i := range dwarfp {
		syms = append(syms, dwarfp[i].syms...)
	}
	writeBlocks(out, ctxt.outSem, syms, addr, size, zeros[:])
}

var zeros [512]byte

var (
	strdata  = make(map[string]string)
	strnames []string
)

func addstrdata1(ctxt *Link, arg string) {
	eq := strings.Index(arg, "=")
	dot := strings.LastIndex(arg[:eq+1], ".")
	if eq < 0 || dot < 0 {
		Exitf("-X flag requires argument of the form importpath.name=value")
	}
	pkg := arg[:dot]
	if ctxt.BuildMode == BuildModePlugin && pkg == "main" {
		pkg = *flagPluginPath
	}
	pkg = objabi.PathToPrefix(pkg)
	name := pkg + arg[dot:eq]
	value := arg[eq+1:]
	if _, ok := strdata[name]; !ok {
		strnames = append(strnames, name)
	}
	strdata[name] = value
}

// addstrdata sets the initial value of the string variable name to value.
func addstrdata(arch *sys.Arch, l *loader.Loader, name, value string) {
	s := l.Lookup(name, 0)
	if s == 0 {
		return
	}
	if goType := l.SymGoType(s); goType == 0 {
		return
	} else if typeName := l.SymName(goType); typeName != "type.string" {
		Errorf(nil, "%s: cannot set with -X: not a var of type string (%s)", name, typeName)
		return
	}
	if !l.AttrReachable(s) {
		return // don't bother setting unreachable variable
	}
	bld := l.MakeSymbolUpdater(s)
	if bld.Type() == sym.SBSS {
		bld.SetType(sym.SDATA)
	}

	p := fmt.Sprintf("%s.str", name)
	sp := l.LookupOrCreateSym(p, 0)
	sbld := l.MakeSymbolUpdater(sp)

	sbld.Addstring(value)
	sbld.SetType(sym.SRODATA)

	bld.SetSize(0)
	bld.SetData(make([]byte, 0, arch.PtrSize*2))
	bld.SetReadOnly(false)
	bld.SetRelocs(nil)
	bld.AddAddrPlus(arch, sp, 0)
	bld.AddUint(arch, uint64(len(value)))
}

func (ctxt *Link) dostrdata() {
	for _, name := range strnames {
		addstrdata(ctxt.Arch, ctxt.loader, name, strdata[name])
	}
}

func Addstring(s *sym.Symbol, str string) int64 {
	if s.Type == 0 {
		s.Type = sym.SNOPTRDATA
	}
	s.Attr |= sym.AttrReachable
	r := s.Size
	if s.Name == ".shstrtab" {
		elfsetstring(s, str, int(r))
	}
	s.P = append(s.P, str...)
	s.P = append(s.P, 0)
	s.Size = int64(len(s.P))
	return r
}

// addgostring adds str, as a Go string value, to s. symname is the name of the
// symbol used to define the string data and must be unique per linked object.
func addgostring(ctxt *Link, ldr *loader.Loader, s *loader.SymbolBuilder, symname, str string) {
	sdata := ldr.CreateSymForUpdate(symname, 0)
	if sdata.Type() != sym.Sxxx {
		ctxt.Errorf(s.Sym(), "duplicate symname in addgostring: %s", symname)
	}
	sdata.SetReachable(true)
	sdata.SetLocal(true)
	sdata.SetType(sym.SRODATA)
	sdata.SetSize(int64(len(str)))
	sdata.SetData([]byte(str))
	s.AddAddr(ctxt.Arch, sdata.Sym())
	s.AddUint(ctxt.Arch, uint64(len(str)))
}

func addinitarrdata(ctxt *Link, ldr *loader.Loader, s loader.Sym) {
	p := ldr.SymName(s) + ".ptr"
	sp := ldr.CreateSymForUpdate(p, 0)
	sp.SetType(sym.SINITARR)
	sp.SetSize(0)
	sp.SetDuplicateOK(true)
	sp.AddAddr(ctxt.Arch, s)
}

// symalign returns the required alignment for the given symbol s.
func symalign(s *sym.Symbol) int32 {
	min := int32(thearch.Minalign)
	if s.Align >= min {
		return s.Align
	} else if s.Align != 0 {
		return min
	}
	if strings.HasPrefix(s.Name, "go.string.") || strings.HasPrefix(s.Name, "type..namedata.") {
		// String data is just bytes.
		// If we align it, we waste a lot of space to padding.
		return min
	}
	align := int32(thearch.Maxalign)
	for int64(align) > s.Size && align > min {
		align >>= 1
	}
	s.Align = align
	return align
}

func aligndatsize(datsize int64, s *sym.Symbol) int64 {
	return Rnd(datsize, int64(symalign(s)))
}

const debugGCProg = false

type GCProg struct {
	ctxt *Link
	sym  *sym.Symbol
	w    gcprog.Writer
}

func (p *GCProg) Init(ctxt *Link, name string) {
	p.ctxt = ctxt
	p.sym = ctxt.Syms.Lookup(name, 0)
	p.w.Init(p.writeByte(ctxt))
	if debugGCProg {
		fmt.Fprintf(os.Stderr, "ld: start GCProg %s\n", name)
		p.w.Debug(os.Stderr)
	}
}

func (p *GCProg) writeByte(ctxt *Link) func(x byte) {
	return func(x byte) {
		p.sym.AddUint8(x)
	}
}

func (p *GCProg) End(size int64) {
	p.w.ZeroUntil(size / int64(p.ctxt.Arch.PtrSize))
	p.w.End()
	if debugGCProg {
		fmt.Fprintf(os.Stderr, "ld: end GCProg\n")
	}
}

func (p *GCProg) AddSym(s *sym.Symbol) {
	typ := s.Gotype
	// Things without pointers should be in sym.SNOPTRDATA or sym.SNOPTRBSS;
	// everything we see should have pointers and should therefore have a type.
	if typ == nil {
		switch s.Name {
		case "runtime.data", "runtime.edata", "runtime.bss", "runtime.ebss":
			// Ignore special symbols that are sometimes laid out
			// as real symbols. See comment about dyld on darwin in
			// the address function.
			return
		}
		Errorf(s, "missing Go type information for global symbol: size %d", s.Size)
		return
	}

	ptrsize := int64(p.ctxt.Arch.PtrSize)
	nptr := decodetypePtrdata(p.ctxt.Arch, typ.P) / ptrsize

	if debugGCProg {
		fmt.Fprintf(os.Stderr, "gcprog sym: %s at %d (ptr=%d+%d)\n", s.Name, s.Value, s.Value/ptrsize, nptr)
	}

	if decodetypeUsegcprog(p.ctxt.Arch, typ.P) == 0 {
		// Copy pointers from mask into program.
		mask := decodetypeGcmask(p.ctxt, typ)
		for i := int64(0); i < nptr; i++ {
			if (mask[i/8]>>uint(i%8))&1 != 0 {
				p.w.Ptr(s.Value/ptrsize + i)
			}
		}
		return
	}

	// Copy program.
	prog := decodetypeGcprog(p.ctxt, typ)
	p.w.ZeroUntil(s.Value / ptrsize)
	p.w.Append(prog[4:], nptr)
}

type GCProg2 struct {
	ctxt *Link
	sym  *loader.SymbolBuilder
	w    gcprog.Writer
}

func (p *GCProg2) Init(ctxt *Link, name string) {
	p.ctxt = ctxt
	symIdx := ctxt.loader.LookupOrCreateSym(name, 0)
	p.sym = ctxt.loader.MakeSymbolUpdater(symIdx)
	p.w.Init(p.writeByte())
	if debugGCProg {
		fmt.Fprintf(os.Stderr, "ld: start GCProg %s\n", name)
		p.w.Debug(os.Stderr)
	}
}

func (p *GCProg2) writeByte() func(x byte) {
	return func(x byte) {
		p.sym.AddUint8(x)
	}
}

func (p *GCProg2) End(size int64) {
	p.w.ZeroUntil(size / int64(p.ctxt.Arch.PtrSize))
	p.w.End()
	if debugGCProg {
		fmt.Fprintf(os.Stderr, "ld: end GCProg\n")
	}
}

func (p *GCProg2) AddSym(s loader.Sym) {
	ldr := p.ctxt.loader
	typ := ldr.SymGoType(s)

	// Things without pointers should be in sym.SNOPTRDATA or sym.SNOPTRBSS;
	// everything we see should have pointers and should therefore have a type.
	if typ == 0 {
		switch p.sym.Name() {
		case "runtime.data", "runtime.edata", "runtime.bss", "runtime.ebss":
			// Ignore special symbols that are sometimes laid out
			// as real symbols. See comment about dyld on darwin in
			// the address function.
			return
		}
		p.ctxt.Errorf(p.sym.Sym(), "missing Go type information for global symbol: size %d", ldr.SymSize(s))
		return
	}

	ptrsize := int64(p.ctxt.Arch.PtrSize)
	typData := ldr.Data(typ)
	nptr := decodetypePtrdata(p.ctxt.Arch, typData) / ptrsize

	if debugGCProg {
		fmt.Fprintf(os.Stderr, "gcprog sym: %s at %d (ptr=%d+%d)\n", ldr.SymName(s), ldr.SymValue(s), ldr.SymValue(s)/ptrsize, nptr)
	}

	sval := ldr.SymValue(s)
	if decodetypeUsegcprog(p.ctxt.Arch, typData) == 0 {
		// Copy pointers from mask into program.
		mask := decodetypeGcmask2(p.ctxt, typ)
		for i := int64(0); i < nptr; i++ {
			if (mask[i/8]>>uint(i%8))&1 != 0 {
				p.w.Ptr(sval/ptrsize + i)
			}
		}
		return
	}

	// Copy program.
	prog := decodetypeGcprog2(p.ctxt, typ)
	p.w.ZeroUntil(sval / ptrsize)
	p.w.Append(prog[4:], nptr)
}

// dataSortKey is used to sort a slice of data symbol *sym.Symbol pointers.
// The sort keys are kept inline to improve cache behavior while sorting.
type dataSortKey struct {
	size int64
	name string
	sym  *sym.Symbol
}

type bySizeAndName []dataSortKey

func (d bySizeAndName) Len() int      { return len(d) }
func (d bySizeAndName) Swap(i, j int) { d[i], d[j] = d[j], d[i] }
func (d bySizeAndName) Less(i, j int) bool {
	s1, s2 := d[i], d[j]
	if s1.size != s2.size {
		return s1.size < s2.size
	}
	return s1.name < s2.name
}

// cutoff is the maximum data section size permitted by the linker
// (see issue #9862).
const cutoff = 2e9 // 2 GB (or so; looks better in errors than 2^31)

func (state *dodataState) checkdatsize(symn sym.SymKind) {
	if state.datsize > cutoff {
		Errorf(nil, "too much data in section %v (over %v bytes)", symn, cutoff)
	}
}

// fixZeroSizedSymbols gives a few special symbols with zero size some space.
func fixZeroSizedSymbols(ctxt *Link) {
	// The values in moduledata are filled out by relocations
	// pointing to the addresses of these special symbols.
	// Typically these symbols have no size and are not laid
	// out with their matching section.
	//
	// However on darwin, dyld will find the special symbol
	// in the first loaded module, even though it is local.
	//
	// (An hypothesis, formed without looking in the dyld sources:
	// these special symbols have no size, so their address
	// matches a real symbol. The dynamic linker assumes we
	// want the normal symbol with the same address and finds
	// it in the other module.)
	//
	// To work around this we lay out the symbls whose
	// addresses are vital for multi-module programs to work
	// as normal symbols, and give them a little size.
	//
	// On AIX, as all DATA sections are merged together, ld might not put
	// these symbols at the beginning of their respective section if there
	// aren't real symbols, their alignment might not match the
	// first symbol alignment. Therefore, there are explicitly put at the
	// beginning of their section with the same alignment.
	if !(ctxt.DynlinkingGo() && ctxt.HeadType == objabi.Hdarwin) && !(ctxt.HeadType == objabi.Haix && ctxt.LinkMode == LinkExternal) {
		return
	}

	bss := ctxt.Syms.Lookup("runtime.bss", 0)
	bss.Size = 8
	bss.Attr.Set(sym.AttrSpecial, false)

	ctxt.Syms.Lookup("runtime.ebss", 0).Attr.Set(sym.AttrSpecial, false)

	data := ctxt.Syms.Lookup("runtime.data", 0)
	data.Size = 8
	data.Attr.Set(sym.AttrSpecial, false)

	edata := ctxt.Syms.Lookup("runtime.edata", 0)
	edata.Attr.Set(sym.AttrSpecial, false)
	if ctxt.HeadType == objabi.Haix {
		// XCOFFTOC symbols are part of .data section.
		edata.Type = sym.SXCOFFTOC
	}

	types := ctxt.Syms.Lookup("runtime.types", 0)
	types.Type = sym.STYPE
	types.Size = 8
	types.Attr.Set(sym.AttrSpecial, false)

	etypes := ctxt.Syms.Lookup("runtime.etypes", 0)
	etypes.Type = sym.SFUNCTAB
	etypes.Attr.Set(sym.AttrSpecial, false)

	if ctxt.HeadType == objabi.Haix {
		rodata := ctxt.Syms.Lookup("runtime.rodata", 0)
		rodata.Type = sym.SSTRING
		rodata.Size = 8
		rodata.Attr.Set(sym.AttrSpecial, false)

		ctxt.Syms.Lookup("runtime.erodata", 0).Attr.Set(sym.AttrSpecial, false)
	}
}

// makeRelroForSharedLib creates a section of readonly data if necessary.
func (state *dodataState) makeRelroForSharedLib(target *Link) {
	if !target.UseRelro() {
		return
	}

	// "read only" data with relocations needs to go in its own section
	// when building a shared library. We do this by boosting objects of
	// type SXXX with relocations to type SXXXRELRO.
	for _, symnro := range sym.ReadOnly {
		symnrelro := sym.RelROMap[symnro]

		ro := []*sym.Symbol{}
		relro := state.data[symnrelro]

		for _, s := range state.data[symnro] {
			isRelro := len(s.R) > 0
			switch s.Type {
			case sym.STYPE, sym.STYPERELRO, sym.SGOFUNCRELRO:
				// Symbols are not sorted yet, so it is possible
				// that an Outer symbol has been changed to a
				// relro Type before it reaches here.
				isRelro = true
			case sym.SFUNCTAB:
				if target.IsAIX() && s.Name == "runtime.etypes" {
					// runtime.etypes must be at the end of
					// the relro datas.
					isRelro = true
				}
			}
			if isRelro {
				s.Type = symnrelro
				if s.Outer != nil {
					s.Outer.Type = s.Type
				}
				relro = append(relro, s)
			} else {
				ro = append(ro, s)
			}
		}

		// Check that we haven't made two symbols with the same .Outer into
		// different types (because references two symbols with non-nil Outer
		// become references to the outer symbol + offset it's vital that the
		// symbol and the outer end up in the same section).
		for _, s := range relro {
			if s.Outer != nil && s.Outer.Type != s.Type {
				Errorf(s, "inconsistent types for symbol and its Outer %s (%v != %v)",
					s.Outer.Name, s.Type, s.Outer.Type)
			}
		}

		state.data[symnro] = ro
		state.data[symnrelro] = relro
	}
}

// dodataState holds bits of state information needed by dodata() and the
// various helpers it calls. The lifetime of these items should not extend
// past the end of dodata().
type dodataState struct {
	// Link context
	ctxt *Link
	// Data symbols bucketed by type.
	data [sym.SXREF][]*sym.Symbol
	// Max alignment for each flavor of data symbol.
	dataMaxAlign [sym.SXREF]int32
	// Current data size so far.
	datsize int64
}

func (ctxt *Link) dodata() {
	// Give zeros sized symbols space if necessary.
	fixZeroSizedSymbols(ctxt)

	// Collect data symbols by type into data.
	state := dodataState{ctxt: ctxt}
	for _, s := range ctxt.Syms.Allsym {
		if !s.Attr.Reachable() || s.Attr.Special() || s.Attr.SubSymbol() {
			continue
		}
		if s.Type <= sym.STEXT || s.Type >= sym.SXREF {
			continue
		}
		state.data[s.Type] = append(state.data[s.Type], s)
	}

	// Now that we have the data symbols, but before we start
	// to assign addresses, record all the necessary
	// dynamic relocations. These will grow the relocation
	// symbol, which is itself data.
	//
	// On darwin, we need the symbol table numbers for dynreloc.
	if ctxt.HeadType == objabi.Hdarwin {
		machosymorder(ctxt)
	}
	state.dynreloc(ctxt)

	// Move any RO data with relocations to a separate section.
	state.makeRelroForSharedLib(ctxt)

	// Sort symbols.
	var wg sync.WaitGroup
	for symn := range state.data {
		symn := sym.SymKind(symn)
		wg.Add(1)
		go func() {
			state.data[symn], state.dataMaxAlign[symn] = dodataSect(ctxt, symn, state.data[symn])
			wg.Done()
		}()
	}
	wg.Wait()

	if ctxt.HeadType == objabi.Haix && ctxt.LinkMode == LinkExternal {
		// These symbols must have the same alignment as their section.
		// Otherwize, ld might change the layout of Go sections.
		ctxt.Syms.ROLookup("runtime.data", 0).Align = state.dataMaxAlign[sym.SDATA]
		ctxt.Syms.ROLookup("runtime.bss", 0).Align = state.dataMaxAlign[sym.SBSS]
	}

	// Create *sym.Section objects and assign symbols to sections for
	// data/rodata (and related) symbols.
	state.allocateDataSections(ctxt)

	// Create *sym.Section objects and assign symbols to sections for
	// DWARF symbols.
	state.allocateDwarfSections(ctxt)

	/* number the sections */
	n := int16(1)

	for _, sect := range Segtext.Sections {
		sect.Extnum = n
		n++
	}
	for _, sect := range Segrodata.Sections {
		sect.Extnum = n
		n++
	}
	for _, sect := range Segrelrodata.Sections {
		sect.Extnum = n
		n++
	}
	for _, sect := range Segdata.Sections {
		sect.Extnum = n
		n++
	}
	for _, sect := range Segdwarf.Sections {
		sect.Extnum = n
		n++
	}
}

// allocateDataSectionForSym creates a new sym.Section into which a a
// single symbol will be placed. Here "seg" is the segment into which
// the section will go, "s" is the symbol to be placed into the new
// section, and "rwx" contains permissions for the section.
func (state *dodataState) allocateDataSectionForSym(seg *sym.Segment, s *sym.Symbol, rwx int) *sym.Section {
	sect := addsection(state.ctxt.loader, state.ctxt.Arch, seg, s.Name, rwx)
	sect.Align = symalign(s)
	state.datsize = Rnd(state.datsize, int64(sect.Align))
	sect.Vaddr = uint64(state.datsize)
	return sect
}

// allocateNamedDataSection creates a new sym.Section for a category
// of data symbols. Here "seg" is the segment into which the section
// will go, "sName" is the name to give to the section, "types" is a
// range of symbol types to be put into the section, and "rwx"
// contains permissions for the section.
func (state *dodataState) allocateNamedDataSection(seg *sym.Segment, sName string, types []sym.SymKind, rwx int) *sym.Section {
	sect := addsection(state.ctxt.loader, state.ctxt.Arch, seg, sName, rwx)
	if len(types) == 0 {
		sect.Align = 1
	} else if len(types) == 1 {
		sect.Align = state.dataMaxAlign[types[0]]
	} else {
		for _, symn := range types {
			align := state.dataMaxAlign[symn]
			if sect.Align < align {
				sect.Align = align
			}
		}
	}
	state.datsize = Rnd(state.datsize, int64(sect.Align))
	sect.Vaddr = uint64(state.datsize)
	return sect
}

// assignDsymsToSection assigns a collection of data symbols to a
// newly created section. "sect" is the section into which to place
// the symbols, "syms" holds the list of symbols to assign,
// "forceType" (if non-zero) contains a new sym type to apply to each
// sym during the assignment, and "aligner" is a hook to call to
// handle alignment during the assignment process.
func (state *dodataState) assignDsymsToSection(sect *sym.Section, syms []*sym.Symbol, forceType sym.SymKind, aligner func(datsize int64, s *sym.Symbol) int64) {
	for _, s := range syms {
		state.datsize = aligner(state.datsize, s)
		s.Sect = sect
		if forceType != sym.Sxxx {
			s.Type = forceType
		}
		s.Value = int64(uint64(state.datsize) - sect.Vaddr)
		state.datsize += s.Size
	}
	sect.Length = uint64(state.datsize) - sect.Vaddr
}

func (state *dodataState) assignToSection(sect *sym.Section, symn sym.SymKind, forceType sym.SymKind) {
	state.assignDsymsToSection(sect, state.data[symn], forceType, aligndatsize)
	state.checkdatsize(symn)
}

// allocateSingleSymSections walks through the bucketed data symbols
// with type 'symn', creates a new section for each sym, and assigns
// the sym to a newly created section. Section name is set from the
// symbol name. "Seg" is the segment into which to place the new
// section, "forceType" is the new sym.SymKind to assign to the symbol
// within the section, and "rwx" holds section permissions.
func (state *dodataState) allocateSingleSymSections(seg *sym.Segment, symn sym.SymKind, forceType sym.SymKind, rwx int) {
	for _, s := range state.data[symn] {
		sect := state.allocateDataSectionForSym(seg, s, rwx)
		s.Sect = sect
		s.Type = forceType
		s.Value = int64(uint64(state.datsize) - sect.Vaddr)
		state.datsize += s.Size
		sect.Length = uint64(state.datsize) - sect.Vaddr
	}
	state.checkdatsize(symn)
}

// allocateNamedSectionAndAssignSyms creates a new section with the
// specified name, then walks through the bucketed data symbols with
// type 'symn' and assigns each of them to this new section. "Seg" is
// the segment into which to place the new section, "secName" is the
// name to give to the new section, "forceType" (if non-zero) contains
// a new sym type to apply to each sym during the assignment, and
// "rwx" holds section permissions.
func (state *dodataState) allocateNamedSectionAndAssignSyms(seg *sym.Segment, secName string, symn sym.SymKind, forceType sym.SymKind, rwx int) *sym.Section {

	sect := state.allocateNamedDataSection(seg, secName, []sym.SymKind{symn}, rwx)
	state.assignDsymsToSection(sect, state.data[symn], forceType, aligndatsize)
	return sect
}

// allocateDataSections allocates sym.Section objects for data/rodata
// (and related) symbols, and then assigns symbols to those sections.
func (state *dodataState) allocateDataSections(ctxt *Link) {
	// Allocate sections.
	// Data is processed before segtext, because we need
	// to see all symbols in the .data and .bss sections in order
	// to generate garbage collection information.

	// Writable data sections that do not need any specialized handling.
	writable := []sym.SymKind{
		sym.SBUILDINFO,
		sym.SELFSECT,
		sym.SMACHO,
		sym.SMACHOGOT,
		sym.SWINDOWS,
	}
	for _, symn := range writable {
		state.allocateSingleSymSections(&Segdata, symn, sym.SDATA, 06)
	}

	// .got (and .toc on ppc64)
	if len(state.data[sym.SELFGOT]) > 0 {
		sect := state.allocateNamedSectionAndAssignSyms(&Segdata, ".got", sym.SELFGOT, sym.SDATA, 06)
		if ctxt.IsPPC64() {
			for _, s := range state.data[sym.SELFGOT] {
				// Resolve .TOC. symbol for this object file (ppc64)
				toc := ctxt.Syms.ROLookup(".TOC.", int(s.Version))
				if toc != nil {
					toc.Sect = sect
					toc.Outer = s
					toc.Sub = s.Sub
					s.Sub = toc

					toc.Value = 0x8000
				}
			}
		}
	}

	/* pointer-free data */
	sect := state.allocateNamedSectionAndAssignSyms(&Segdata, ".noptrdata", sym.SNOPTRDATA, sym.SDATA, 06)
	ctxt.Syms.Lookup("runtime.noptrdata", 0).Sect = sect
	ctxt.Syms.Lookup("runtime.enoptrdata", 0).Sect = sect

	hasinitarr := ctxt.linkShared

	/* shared library initializer */
	switch ctxt.BuildMode {
	case BuildModeCArchive, BuildModeCShared, BuildModeShared, BuildModePlugin:
		hasinitarr = true
	}

	if ctxt.HeadType == objabi.Haix {
		if len(state.data[sym.SINITARR]) > 0 {
			Errorf(nil, "XCOFF format doesn't allow .init_array section")
		}
	}

	if hasinitarr && len(state.data[sym.SINITARR]) > 0 {
		state.allocateNamedSectionAndAssignSyms(&Segdata, ".init_array", sym.SINITARR, sym.Sxxx, 06)
	}

	/* data */
	sect = state.allocateNamedSectionAndAssignSyms(&Segdata, ".data", sym.SDATA, sym.SDATA, 06)
	ctxt.Syms.Lookup("runtime.data", 0).Sect = sect
	ctxt.Syms.Lookup("runtime.edata", 0).Sect = sect
	dataGcEnd := state.datsize - int64(sect.Vaddr)

	// On AIX, TOC entries must be the last of .data
	// These aren't part of gc as they won't change during the runtime.
	state.assignToSection(sect, sym.SXCOFFTOC, sym.SDATA)
	state.checkdatsize(sym.SDATA)
	sect.Length = uint64(state.datsize) - sect.Vaddr

	/* bss */
	sect = state.allocateNamedSectionAndAssignSyms(&Segdata, ".bss", sym.SBSS, sym.Sxxx, 06)
	ctxt.Syms.Lookup("runtime.bss", 0).Sect = sect
	ctxt.Syms.Lookup("runtime.ebss", 0).Sect = sect
	bssGcEnd := state.datsize - int64(sect.Vaddr)

	// Emit gcdata for bcc symbols now that symbol values have been assigned.
	gcsToEmit := []struct {
		symName string
		symKind sym.SymKind
		gcEnd   int64
	}{
		{"runtime.gcdata", sym.SDATA, dataGcEnd},
		{"runtime.gcbss", sym.SBSS, bssGcEnd},
	}
	for _, g := range gcsToEmit {
		var gc GCProg
		gc.Init(ctxt, g.symName)
		for _, s := range state.data[g.symKind] {
			gc.AddSym(s)
		}
		gc.End(g.gcEnd)
	}

	/* pointer-free bss */
	sect = state.allocateNamedSectionAndAssignSyms(&Segdata, ".noptrbss", sym.SNOPTRBSS, sym.Sxxx, 06)
	ctxt.Syms.Lookup("runtime.noptrbss", 0).Sect = sect
	ctxt.Syms.Lookup("runtime.enoptrbss", 0).Sect = sect
	ctxt.Syms.Lookup("runtime.end", 0).Sect = sect

	// Coverage instrumentation counters for libfuzzer.
	if len(state.data[sym.SLIBFUZZER_EXTRA_COUNTER]) > 0 {
		state.allocateNamedSectionAndAssignSyms(&Segdata, "__libfuzzer_extra_counters", sym.SLIBFUZZER_EXTRA_COUNTER, sym.Sxxx, 06)
	}

	if len(state.data[sym.STLSBSS]) > 0 {
		var sect *sym.Section
		// FIXME: not clear why it is sometimes necessary to suppress .tbss section creation.
		if (ctxt.IsELF || ctxt.HeadType == objabi.Haix) && (ctxt.LinkMode == LinkExternal || !*FlagD) {
			sect = addsection(ctxt.loader, ctxt.Arch, &Segdata, ".tbss", 06)
			sect.Align = int32(ctxt.Arch.PtrSize)
			// FIXME: why does this need to be set to zero?
			sect.Vaddr = 0
		}
		state.datsize = 0

		for _, s := range state.data[sym.STLSBSS] {
			state.datsize = aligndatsize(state.datsize, s)
			s.Sect = sect
			s.Value = state.datsize
			state.datsize += s.Size
		}
		state.checkdatsize(sym.STLSBSS)

		if sect != nil {
			sect.Length = uint64(state.datsize)
		}
	}

	/*
	 * We finished data, begin read-only data.
	 * Not all systems support a separate read-only non-executable data section.
	 * ELF and Windows PE systems do.
	 * OS X and Plan 9 do not.
	 * And if we're using external linking mode, the point is moot,
	 * since it's not our decision; that code expects the sections in
	 * segtext.
	 */
	var segro *sym.Segment
	if ctxt.IsELF && ctxt.LinkMode == LinkInternal {
		segro = &Segrodata
	} else if ctxt.HeadType == objabi.Hwindows {
		segro = &Segrodata
	} else {
		segro = &Segtext
	}

	state.datsize = 0

	/* read-only executable ELF, Mach-O sections */
	if len(state.data[sym.STEXT]) != 0 {
		Errorf(nil, "dodata found an sym.STEXT symbol: %s", state.data[sym.STEXT][0].Name)
	}
	state.allocateSingleSymSections(&Segtext, sym.SELFRXSECT, sym.SRODATA, 04)

	/* read-only data */
	sect = state.allocateNamedDataSection(segro, ".rodata", sym.ReadOnly, 04)
	ctxt.Syms.Lookup("runtime.rodata", 0).Sect = sect
	ctxt.Syms.Lookup("runtime.erodata", 0).Sect = sect
	if !ctxt.UseRelro() {
		ctxt.Syms.Lookup("runtime.types", 0).Sect = sect
		ctxt.Syms.Lookup("runtime.etypes", 0).Sect = sect
	}
	for _, symn := range sym.ReadOnly {
		symnStartValue := state.datsize
		state.assignToSection(sect, symn, sym.SRODATA)
		if ctxt.HeadType == objabi.Haix {
			// Read-only symbols might be wrapped inside their outer
			// symbol.
			// XCOFF symbol table needs to know the size of
			// these outer symbols.
			xcoffUpdateOuterSize(ctxt, state.datsize-symnStartValue, symn)
		}
	}

	/* read-only ELF, Mach-O sections */
	state.allocateSingleSymSections(segro, sym.SELFROSECT, sym.SRODATA, 04)
	state.allocateSingleSymSections(segro, sym.SMACHOPLT, sym.SRODATA, 04)

	// There is some data that are conceptually read-only but are written to by
	// relocations. On GNU systems, we can arrange for the dynamic linker to
	// mprotect sections after relocations are applied by giving them write
	// permissions in the object file and calling them ".data.rel.ro.FOO". We
	// divide the .rodata section between actual .rodata and .data.rel.ro.rodata,
	// but for the other sections that this applies to, we just write a read-only
	// .FOO section or a read-write .data.rel.ro.FOO section depending on the
	// situation.
	// TODO(mwhudson): It would make sense to do this more widely, but it makes
	// the system linker segfault on darwin.
	const relroPerm = 06
	const fallbackPerm = 04
	relroSecPerm := fallbackPerm
	genrelrosecname := func(suffix string) string {
		return suffix
	}
	seg := segro

	if ctxt.UseRelro() {
		segrelro := &Segrelrodata
		if ctxt.LinkMode == LinkExternal && ctxt.HeadType != objabi.Haix {
			// Using a separate segment with an external
			// linker results in some programs moving
			// their data sections unexpectedly, which
			// corrupts the moduledata. So we use the
			// rodata segment and let the external linker
			// sort out a rel.ro segment.
			segrelro = segro
		} else {
			// Reset datsize for new segment.
			state.datsize = 0
		}

		genrelrosecname = func(suffix string) string {
			return ".data.rel.ro" + suffix
		}
		relroReadOnly := []sym.SymKind{}
		for _, symnro := range sym.ReadOnly {
			symn := sym.RelROMap[symnro]
			relroReadOnly = append(relroReadOnly, symn)
		}
		seg = segrelro
		relroSecPerm = relroPerm

		/* data only written by relocations */
		sect = state.allocateNamedDataSection(segrelro, genrelrosecname(""), relroReadOnly, relroSecPerm)

		ctxt.Syms.Lookup("runtime.types", 0).Sect = sect
		ctxt.Syms.Lookup("runtime.etypes", 0).Sect = sect

		for i, symnro := range sym.ReadOnly {
			if i == 0 && symnro == sym.STYPE && ctxt.HeadType != objabi.Haix {
				// Skip forward so that no type
				// reference uses a zero offset.
				// This is unlikely but possible in small
				// programs with no other read-only data.
				state.datsize++
			}

			symn := sym.RelROMap[symnro]
			symnStartValue := state.datsize

			for _, s := range state.data[symn] {
				if s.Outer != nil && s.Outer.Sect != nil && s.Outer.Sect != sect {
					Errorf(s, "s.Outer (%s) in different section from s, %s != %s", s.Outer.Name, s.Outer.Sect.Name, sect.Name)
				}
			}
			state.assignToSection(sect, symn, sym.SRODATA)
			if ctxt.HeadType == objabi.Haix {
				// Read-only symbols might be wrapped inside their outer
				// symbol.
				// XCOFF symbol table needs to know the size of
				// these outer symbols.
				xcoffUpdateOuterSize(ctxt, state.datsize-symnStartValue, symn)
			}
		}

		sect.Length = uint64(state.datsize) - sect.Vaddr
	}

	/* typelink */
	sect = state.allocateNamedDataSection(seg, genrelrosecname(".typelink"), []sym.SymKind{sym.STYPELINK}, relroSecPerm)
	typelink := ctxt.Syms.Lookup("runtime.typelink", 0)
	typelink.Sect = sect
	typelink.Type = sym.SRODATA
	state.datsize += typelink.Size
	state.checkdatsize(sym.STYPELINK)
	sect.Length = uint64(state.datsize) - sect.Vaddr

	/* itablink */
	sect = state.allocateNamedSectionAndAssignSyms(seg, genrelrosecname(".itablink"), sym.SITABLINK, sym.Sxxx, relroSecPerm)
	ctxt.Syms.Lookup("runtime.itablink", 0).Sect = sect
	ctxt.Syms.Lookup("runtime.eitablink", 0).Sect = sect
	if ctxt.HeadType == objabi.Haix {
		// Store .itablink size because its symbols are wrapped
		// under an outer symbol: runtime.itablink.
		xcoffUpdateOuterSize(ctxt, int64(sect.Length), sym.SITABLINK)
	}

	/* gosymtab */
	sect = state.allocateNamedSectionAndAssignSyms(seg, genrelrosecname(".gosymtab"), sym.SSYMTAB, sym.SRODATA, relroSecPerm)
	ctxt.Syms.Lookup("runtime.symtab", 0).Sect = sect
	ctxt.Syms.Lookup("runtime.esymtab", 0).Sect = sect

	/* gopclntab */
	sect = state.allocateNamedSectionAndAssignSyms(seg, genrelrosecname(".gopclntab"), sym.SPCLNTAB, sym.SRODATA, relroSecPerm)
	ctxt.Syms.Lookup("runtime.pclntab", 0).Sect = sect
	ctxt.Syms.Lookup("runtime.epclntab", 0).Sect = sect

	// 6g uses 4-byte relocation offsets, so the entire segment must fit in 32 bits.
	if state.datsize != int64(uint32(state.datsize)) {
		Errorf(nil, "read-only data segment too large: %d", state.datsize)
	}

	for symn := sym.SELFRXSECT; symn < sym.SXREF; symn++ {
		ctxt.datap = append(ctxt.datap, state.data[symn]...)
	}
}

// allocateDwarfSections allocates sym.Section objects for DWARF
// symbols, and assigns symbols to sections.
func (state *dodataState) allocateDwarfSections(ctxt *Link) {

	alignOne := func(datsize int64, s *sym.Symbol) int64 { return datsize }

	for i := 0; i < len(dwarfp); i++ {
		// First the section symbol.
		s := dwarfp[i].secSym()
		sect := state.allocateNamedDataSection(&Segdwarf, s.Name, []sym.SymKind{}, 04)
		sect.Sym = s
		s.Sect = sect
		s.Type = sym.SRODATA
		s.Value = int64(uint64(state.datsize) - sect.Vaddr)
		state.datsize += s.Size
		curType := s.Type

		// Then any sub-symbols for the section symbol.
		subSyms := dwarfp[i].subSyms()
		state.assignDsymsToSection(sect, subSyms, sym.SRODATA, alignOne)

		for j := 0; j < len(subSyms); j++ {
			s := subSyms[j]
			if ctxt.HeadType == objabi.Haix && curType == sym.SDWARFLOC {
				// Update the size of .debug_loc for this symbol's
				// package.
				addDwsectCUSize(".debug_loc", s.File, uint64(s.Size))
			}
		}
		sect.Length = uint64(state.datsize) - sect.Vaddr
		state.checkdatsize(curType)
	}
}

func dodataSect(ctxt *Link, symn sym.SymKind, syms []*sym.Symbol) (result []*sym.Symbol, maxAlign int32) {
	if ctxt.HeadType == objabi.Hdarwin {
		// Some symbols may no longer belong in syms
		// due to movement in machosymorder.
		newSyms := make([]*sym.Symbol, 0, len(syms))
		for _, s := range syms {
			if s.Type == symn {
				newSyms = append(newSyms, s)
			}
		}
		syms = newSyms
	}

	var head, tail *sym.Symbol
	symsSort := make([]dataSortKey, 0, len(syms))
	for _, s := range syms {
		if s.Attr.OnList() {
			log.Fatalf("symbol %s listed multiple times", s.Name)
		}
		s.Attr |= sym.AttrOnList
		switch {
		case s.Size < int64(len(s.P)):
			Errorf(s, "initialize bounds (%d < %d)", s.Size, len(s.P))
		case s.Size < 0:
			Errorf(s, "negative size (%d bytes)", s.Size)
		case s.Size > cutoff:
			Errorf(s, "symbol too large (%d bytes)", s.Size)
		}

		// If the usually-special section-marker symbols are being laid
		// out as regular symbols, put them either at the beginning or
		// end of their section.
		if (ctxt.DynlinkingGo() && ctxt.HeadType == objabi.Hdarwin) || (ctxt.HeadType == objabi.Haix && ctxt.LinkMode == LinkExternal) {
			switch s.Name {
			case "runtime.text", "runtime.bss", "runtime.data", "runtime.types", "runtime.rodata":
				head = s
				continue
			case "runtime.etext", "runtime.ebss", "runtime.edata", "runtime.etypes", "runtime.erodata":
				tail = s
				continue
			}
		}

		key := dataSortKey{
			size: s.Size,
			name: s.Name,
			sym:  s,
		}

		switch s.Type {
		case sym.SELFGOT:
			// For ppc64, we want to interleave the .got and .toc sections
			// from input files. Both are type sym.SELFGOT, so in that case
			// we skip size comparison and fall through to the name
			// comparison (conveniently, .got sorts before .toc).
			key.size = 0
		}

		symsSort = append(symsSort, key)
	}

	sort.Sort(bySizeAndName(symsSort))

	off := 0
	if head != nil {
		syms[0] = head
		off++
	}
	for i, symSort := range symsSort {
		syms[i+off] = symSort.sym
		align := symalign(symSort.sym)
		if maxAlign < align {
			maxAlign = align
		}
	}
	if tail != nil {
		syms[len(syms)-1] = tail
	}

	if ctxt.IsELF && symn == sym.SELFROSECT {
		// Make .rela and .rela.plt contiguous, the ELF ABI requires this
		// and Solaris actually cares.
		reli, plti := -1, -1
		for i, s := range syms {
			switch s.Name {
			case ".rel.plt", ".rela.plt":
				plti = i
			case ".rel", ".rela":
				reli = i
			}
		}
		if reli >= 0 && plti >= 0 && plti != reli+1 {
			var first, second int
			if plti > reli {
				first, second = reli, plti
			} else {
				first, second = plti, reli
			}
			rel, plt := syms[reli], syms[plti]
			copy(syms[first+2:], syms[first+1:second])
			syms[first+0] = rel
			syms[first+1] = plt

			// Make sure alignment doesn't introduce a gap.
			// Setting the alignment explicitly prevents
			// symalign from basing it on the size and
			// getting it wrong.
			rel.Align = int32(ctxt.Arch.RegSize)
			plt.Align = int32(ctxt.Arch.RegSize)
		}
	}

	return syms, maxAlign
}

// Add buildid to beginning of text segment, on non-ELF systems.
// Non-ELF binary formats are not always flexible enough to
// give us a place to put the Go build ID. On those systems, we put it
// at the very beginning of the text segment.
// This ``header'' is read by cmd/go.
func (ctxt *Link) textbuildid() {
	if ctxt.IsELF || ctxt.BuildMode == BuildModePlugin || *flagBuildid == "" {
		return
	}

	ldr := ctxt.loader
	s := ldr.CreateSymForUpdate("go.buildid", 0)
	s.SetReachable(true)
	// The \xff is invalid UTF-8, meant to make it less likely
	// to find one of these accidentally.
	data := "\xff Go build ID: " + strconv.Quote(*flagBuildid) + "\n \xff"
	s.SetType(sym.STEXT)
	s.SetData([]byte(data))
	s.SetSize(int64(len(data)))

	ctxt.Textp2 = append(ctxt.Textp2, 0)
	copy(ctxt.Textp2[1:], ctxt.Textp2)
	ctxt.Textp2[0] = s.Sym()
}

func (ctxt *Link) buildinfo() {
	if ctxt.linkShared || ctxt.BuildMode == BuildModePlugin {
		// -linkshared and -buildmode=plugin get confused
		// about the relocations in go.buildinfo
		// pointing at the other data sections.
		// The version information is only available in executables.
		return
	}

	ldr := ctxt.loader
	s := ldr.CreateSymForUpdate(".go.buildinfo", 0)
	s.SetReachable(true)
	s.SetType(sym.SBUILDINFO)
	s.SetAlign(16)
	// The \xff is invalid UTF-8, meant to make it less likely
	// to find one of these accidentally.
	const prefix = "\xff Go buildinf:" // 14 bytes, plus 2 data bytes filled in below
	data := make([]byte, 32)
	copy(data, prefix)
	data[len(prefix)] = byte(ctxt.Arch.PtrSize)
	data[len(prefix)+1] = 0
	if ctxt.Arch.ByteOrder == binary.BigEndian {
		data[len(prefix)+1] = 1
	}
	s.SetData(data)
	s.SetSize(int64(len(data)))
	r, _ := s.AddRel(objabi.R_ADDR)
	r.SetOff(16)
	r.SetSiz(uint8(ctxt.Arch.PtrSize))
	r.SetSym(ldr.LookupOrCreateSym("runtime.buildVersion", 0))
	r, _ = s.AddRel(objabi.R_ADDR)
	r.SetOff(16 + int32(ctxt.Arch.PtrSize))
	r.SetSiz(uint8(ctxt.Arch.PtrSize))
	r.SetSym(ldr.LookupOrCreateSym("runtime.modinfo", 0))
}

// assign addresses to text
func (ctxt *Link) textaddress() {
	addsection(ctxt.loader, ctxt.Arch, &Segtext, ".text", 05)

	// Assign PCs in text segment.
	// Could parallelize, by assigning to text
	// and then letting threads copy down, but probably not worth it.
	sect := Segtext.Sections[0]

	sect.Align = int32(Funcalign)

	ldr := ctxt.loader
	text := ldr.LookupOrCreateSym("runtime.text", 0)
	ldr.SetAttrReachable(text, true)
	ldr.SetSymSect(text, sect)
	if ctxt.IsAIX() && ctxt.IsExternal() {
		// Setting runtime.text has a real symbol prevents ld to
		// change its base address resulting in wrong offsets for
		// reflect methods.
		u := ldr.MakeSymbolUpdater(text)
		u.SetAlign(sect.Align)
		u.SetSize(8)
	}

	if (ctxt.DynlinkingGo() && ctxt.IsDarwin()) || (ctxt.IsAIX() && ctxt.IsExternal()) {
		etext := ldr.LookupOrCreateSym("runtime.etext", 0)
		ldr.SetSymSect(etext, sect)

		ctxt.Textp2 = append(ctxt.Textp2, etext, 0)
		copy(ctxt.Textp2[1:], ctxt.Textp2)
		ctxt.Textp2[0] = text
	}

	va := uint64(*FlagTextAddr)
	n := 1
	sect.Vaddr = va
	ntramps := 0
	for _, s := range ctxt.Textp2 {
		sect, n, va = assignAddress(ctxt, sect, n, s, va, false)

		trampoline(ctxt, s) // resolve jumps, may add trampolines if jump too far

		// lay down trampolines after each function
		for ; ntramps < len(ctxt.tramps); ntramps++ {
			tramp := ctxt.tramps[ntramps]
			if ctxt.IsAIX() && strings.HasPrefix(ldr.SymName(tramp), "runtime.text.") {
				// Already set in assignAddress
				continue
			}
			sect, n, va = assignAddress(ctxt, sect, n, tramp, va, true)
		}
	}

	sect.Length = va - sect.Vaddr
	etext := ldr.LookupOrCreateSym("runtime.etext", 0)
	ldr.SetAttrReachable(etext, true)
	ldr.SetSymSect(etext, sect)

	// merge tramps into Textp, keeping Textp in address order
	if ntramps != 0 {
		newtextp := make([]loader.Sym, 0, len(ctxt.Textp)+ntramps)
		i := 0
		for _, s := range ctxt.Textp2 {
			for ; i < ntramps && ldr.SymValue(ctxt.tramps[i]) < ldr.SymValue(s); i++ {
				newtextp = append(newtextp, ctxt.tramps[i])
			}
			newtextp = append(newtextp, s)
		}
		newtextp = append(newtextp, ctxt.tramps[i:ntramps]...)

		ctxt.Textp2 = newtextp
	}
}

// assigns address for a text symbol, returns (possibly new) section, its number, and the address
func assignAddress(ctxt *Link, sect *sym.Section, n int, s loader.Sym, va uint64, isTramp bool) (*sym.Section, int, uint64) {
	ldr := ctxt.loader
	if thearch.AssignAddress != nil {
		return thearch.AssignAddress(ldr, sect, n, s, va, isTramp)
	}

	ldr.SetSymSect(s, sect)
	if ldr.AttrSubSymbol(s) {
		return sect, n, va
	}

	align := ldr.SymAlign(s)
	if align == 0 {
		align = int32(Funcalign)
	}
	va = uint64(Rnd(int64(va), int64(align)))
	if sect.Align < align {
		sect.Align = align
	}

	funcsize := uint64(MINFUNC) // spacing required for findfunctab
	if ldr.SymSize(s) > MINFUNC {
		funcsize = uint64(ldr.SymSize(s))
	}

	// On ppc64x a text section should not be larger than 2^26 bytes due to the size of
	// call target offset field in the bl instruction.  Splitting into smaller text
	// sections smaller than this limit allows the GNU linker to modify the long calls
	// appropriately.  The limit allows for the space needed for tables inserted by the linker.

	// If this function doesn't fit in the current text section, then create a new one.

	// Only break at outermost syms.

	if ctxt.Arch.InFamily(sys.PPC64) && ldr.OuterSym(s) == 0 && ctxt.IsExternal() && va-sect.Vaddr+funcsize+maxSizeTrampolinesPPC64(ldr, s, isTramp) > 0x1c00000 {
		// Set the length for the previous text section
		sect.Length = va - sect.Vaddr

		// Create new section, set the starting Vaddr
		sect = addsection(ctxt.loader, ctxt.Arch, &Segtext, ".text", 05)
		sect.Vaddr = va
		ldr.SetSymSect(s, sect)

		// Create a symbol for the start of the secondary text sections
		ntext := ldr.CreateSymForUpdate(fmt.Sprintf("runtime.text.%d", n), 0)
		ntext.SetReachable(true)
		ntext.SetSect(sect)
		if ctxt.IsAIX() {
			// runtime.text.X must be a real symbol on AIX.
			// Assign its address directly in order to be the
			// first symbol of this new section.
			ntext.SetType(sym.STEXT)
			ntext.SetSize(int64(MINFUNC))
			ntext.SetOnList(true)
			ctxt.tramps = append(ctxt.tramps, ntext.Sym())

			ntext.SetValue(int64(va))
			va += uint64(ntext.Size())

			if align := ldr.SymAlign(s); align != 0 {
				va = uint64(Rnd(int64(va), int64(align)))
			} else {
				va = uint64(Rnd(int64(va), int64(Funcalign)))
			}
		}
		n++
	}

	ldr.SetSymValue(s, 0)
	for sub := s; sub != 0; sub = ldr.SubSym(sub) {
		ldr.SetSymValue(sub, ldr.SymValue(sub)+int64(va))
	}

	va += funcsize

	return sect, n, va
}

// address assigns virtual addresses to all segments and sections and
// returns all segments in file order.
func (ctxt *Link) address() []*sym.Segment {
	var order []*sym.Segment // Layout order

	va := uint64(*FlagTextAddr)
	order = append(order, &Segtext)
	Segtext.Rwx = 05
	Segtext.Vaddr = va
	for _, s := range Segtext.Sections {
		va = uint64(Rnd(int64(va), int64(s.Align)))
		s.Vaddr = va
		va += s.Length
	}

	Segtext.Length = va - uint64(*FlagTextAddr)

	if len(Segrodata.Sections) > 0 {
		// align to page boundary so as not to mix
		// rodata and executable text.
		//
		// Note: gold or GNU ld will reduce the size of the executable
		// file by arranging for the relro segment to end at a page
		// boundary, and overlap the end of the text segment with the
		// start of the relro segment in the file.  The PT_LOAD segments
		// will be such that the last page of the text segment will be
		// mapped twice, once r-x and once starting out rw- and, after
		// relocation processing, changed to r--.
		//
		// Ideally the last page of the text segment would not be
		// writable even for this short period.
		va = uint64(Rnd(int64(va), int64(*FlagRound)))

		order = append(order, &Segrodata)
		Segrodata.Rwx = 04
		Segrodata.Vaddr = va
		for _, s := range Segrodata.Sections {
			va = uint64(Rnd(int64(va), int64(s.Align)))
			s.Vaddr = va
			va += s.Length
		}

		Segrodata.Length = va - Segrodata.Vaddr
	}
	if len(Segrelrodata.Sections) > 0 {
		// align to page boundary so as not to mix
		// rodata, rel-ro data, and executable text.
		va = uint64(Rnd(int64(va), int64(*FlagRound)))
		if ctxt.HeadType == objabi.Haix {
			// Relro data are inside data segment on AIX.
			va += uint64(XCOFFDATABASE) - uint64(XCOFFTEXTBASE)
		}

		order = append(order, &Segrelrodata)
		Segrelrodata.Rwx = 06
		Segrelrodata.Vaddr = va
		for _, s := range Segrelrodata.Sections {
			va = uint64(Rnd(int64(va), int64(s.Align)))
			s.Vaddr = va
			va += s.Length
		}

		Segrelrodata.Length = va - Segrelrodata.Vaddr
	}

	va = uint64(Rnd(int64(va), int64(*FlagRound)))
	if ctxt.HeadType == objabi.Haix && len(Segrelrodata.Sections) == 0 {
		// Data sections are moved to an unreachable segment
		// to ensure that they are position-independent.
		// Already done if relro sections exist.
		va += uint64(XCOFFDATABASE) - uint64(XCOFFTEXTBASE)
	}
	order = append(order, &Segdata)
	Segdata.Rwx = 06
	Segdata.Vaddr = va
	var data *sym.Section
	var noptr *sym.Section
	var bss *sym.Section
	var noptrbss *sym.Section
	for i, s := range Segdata.Sections {
		if (ctxt.IsELF || ctxt.HeadType == objabi.Haix) && s.Name == ".tbss" {
			continue
		}
		vlen := int64(s.Length)
		if i+1 < len(Segdata.Sections) && !((ctxt.IsELF || ctxt.HeadType == objabi.Haix) && Segdata.Sections[i+1].Name == ".tbss") {
			vlen = int64(Segdata.Sections[i+1].Vaddr - s.Vaddr)
		}
		s.Vaddr = va
		va += uint64(vlen)
		Segdata.Length = va - Segdata.Vaddr
		if s.Name == ".data" {
			data = s
		}
		if s.Name == ".noptrdata" {
			noptr = s
		}
		if s.Name == ".bss" {
			bss = s
		}
		if s.Name == ".noptrbss" {
			noptrbss = s
		}
	}

	// Assign Segdata's Filelen omitting the BSS. We do this here
	// simply because right now we know where the BSS starts.
	Segdata.Filelen = bss.Vaddr - Segdata.Vaddr

	va = uint64(Rnd(int64(va), int64(*FlagRound)))
	order = append(order, &Segdwarf)
	Segdwarf.Rwx = 06
	Segdwarf.Vaddr = va
	for i, s := range Segdwarf.Sections {
		vlen := int64(s.Length)
		if i+1 < len(Segdwarf.Sections) {
			vlen = int64(Segdwarf.Sections[i+1].Vaddr - s.Vaddr)
		}
		s.Vaddr = va
		va += uint64(vlen)
		if ctxt.HeadType == objabi.Hwindows {
			va = uint64(Rnd(int64(va), PEFILEALIGN))
		}
		Segdwarf.Length = va - Segdwarf.Vaddr
	}

	var (
		text     = Segtext.Sections[0]
		rodata   = ctxt.Syms.Lookup("runtime.rodata", 0).Sect
		itablink = ctxt.Syms.Lookup("runtime.itablink", 0).Sect
		symtab   = ctxt.Syms.Lookup("runtime.symtab", 0).Sect
		pclntab  = ctxt.Syms.Lookup("runtime.pclntab", 0).Sect
		types    = ctxt.Syms.Lookup("runtime.types", 0).Sect
	)
	lasttext := text
	// Could be multiple .text sections
	for _, sect := range Segtext.Sections {
		if sect.Name == ".text" {
			lasttext = sect
		}
	}

	for _, s := range ctxt.datap {
		if s.Sect != nil {
			s.Value += int64(s.Sect.Vaddr)
		}
		for sub := s.Sub; sub != nil; sub = sub.Sub {
			sub.Value += s.Value
		}
	}

	for _, si := range dwarfp {
		for _, s := range si.syms {
			if s.Sect != nil {
				s.Value += int64(s.Sect.Vaddr)
			}
			if s.Sub != nil {
				panic(fmt.Sprintf("unexpected sub-sym for %s %s", s.Name, s.Type.String()))
			}
			for sub := s.Sub; sub != nil; sub = sub.Sub {
				sub.Value += s.Value
			}
		}
	}

	if ctxt.BuildMode == BuildModeShared {
		s := ctxt.Syms.Lookup("go.link.abihashbytes", 0)
		sectSym := ctxt.Syms.Lookup(".note.go.abihash", 0)
		s.Sect = sectSym.Sect
		s.Value = int64(sectSym.Sect.Vaddr + 16)
	}

	ctxt.xdefine("runtime.text", sym.STEXT, int64(text.Vaddr))
	ctxt.xdefine("runtime.etext", sym.STEXT, int64(lasttext.Vaddr+lasttext.Length))

	// If there are multiple text sections, create runtime.text.n for
	// their section Vaddr, using n for index
	n := 1
	for _, sect := range Segtext.Sections[1:] {
		if sect.Name != ".text" {
			break
		}
		symname := fmt.Sprintf("runtime.text.%d", n)
		if ctxt.HeadType != objabi.Haix || ctxt.LinkMode != LinkExternal {
			// Addresses are already set on AIX with external linker
			// because these symbols are part of their sections.
			ctxt.xdefine(symname, sym.STEXT, int64(sect.Vaddr))
		}
		n++
	}

	ctxt.xdefine("runtime.rodata", sym.SRODATA, int64(rodata.Vaddr))
	ctxt.xdefine("runtime.erodata", sym.SRODATA, int64(rodata.Vaddr+rodata.Length))
	ctxt.xdefine("runtime.types", sym.SRODATA, int64(types.Vaddr))
	ctxt.xdefine("runtime.etypes", sym.SRODATA, int64(types.Vaddr+types.Length))
	ctxt.xdefine("runtime.itablink", sym.SRODATA, int64(itablink.Vaddr))
	ctxt.xdefine("runtime.eitablink", sym.SRODATA, int64(itablink.Vaddr+itablink.Length))

	s := ctxt.Syms.Lookup("runtime.gcdata", 0)
	s.Attr |= sym.AttrLocal
	ctxt.xdefine("runtime.egcdata", sym.SRODATA, Symaddr(s)+s.Size)
	ctxt.Syms.Lookup("runtime.egcdata", 0).Sect = s.Sect

	s = ctxt.Syms.Lookup("runtime.gcbss", 0)
	s.Attr |= sym.AttrLocal
	ctxt.xdefine("runtime.egcbss", sym.SRODATA, Symaddr(s)+s.Size)
	ctxt.Syms.Lookup("runtime.egcbss", 0).Sect = s.Sect

	ctxt.xdefine("runtime.symtab", sym.SRODATA, int64(symtab.Vaddr))
	ctxt.xdefine("runtime.esymtab", sym.SRODATA, int64(symtab.Vaddr+symtab.Length))
	ctxt.xdefine("runtime.pclntab", sym.SRODATA, int64(pclntab.Vaddr))
	ctxt.xdefine("runtime.epclntab", sym.SRODATA, int64(pclntab.Vaddr+pclntab.Length))
	ctxt.xdefine("runtime.noptrdata", sym.SNOPTRDATA, int64(noptr.Vaddr))
	ctxt.xdefine("runtime.enoptrdata", sym.SNOPTRDATA, int64(noptr.Vaddr+noptr.Length))
	ctxt.xdefine("runtime.bss", sym.SBSS, int64(bss.Vaddr))
	ctxt.xdefine("runtime.ebss", sym.SBSS, int64(bss.Vaddr+bss.Length))
	ctxt.xdefine("runtime.data", sym.SDATA, int64(data.Vaddr))
	ctxt.xdefine("runtime.edata", sym.SDATA, int64(data.Vaddr+data.Length))
	ctxt.xdefine("runtime.noptrbss", sym.SNOPTRBSS, int64(noptrbss.Vaddr))
	ctxt.xdefine("runtime.enoptrbss", sym.SNOPTRBSS, int64(noptrbss.Vaddr+noptrbss.Length))
	ctxt.xdefine("runtime.end", sym.SBSS, int64(Segdata.Vaddr+Segdata.Length))

	if ctxt.IsSolaris() {
		// On Solaris, in the runtime it sets the external names of the
		// end symbols. Unset them and define separate symbols, so we
		// keep both.
		etext := ctxt.Syms.ROLookup("runtime.etext", 0)
		edata := ctxt.Syms.ROLookup("runtime.edata", 0)
		end := ctxt.Syms.ROLookup("runtime.end", 0)
		etext.SetExtname("runtime.etext")
		edata.SetExtname("runtime.edata")
		end.SetExtname("runtime.end")
		ctxt.xdefine("_etext", etext.Type, etext.Value)
		ctxt.xdefine("_edata", edata.Type, edata.Value)
		ctxt.xdefine("_end", end.Type, end.Value)
		ctxt.Syms.ROLookup("_etext", 0).Sect = etext.Sect
		ctxt.Syms.ROLookup("_edata", 0).Sect = edata.Sect
		ctxt.Syms.ROLookup("_end", 0).Sect = end.Sect
	}

	return order
}

// layout assigns file offsets and lengths to the segments in order.
// Returns the file size containing all the segments.
func (ctxt *Link) layout(order []*sym.Segment) uint64 {
	var prev *sym.Segment
	for _, seg := range order {
		if prev == nil {
			seg.Fileoff = uint64(HEADR)
		} else {
			switch ctxt.HeadType {
			default:
				// Assuming the previous segment was
				// aligned, the following rounding
				// should ensure that this segment's
				// VA ≡ Fileoff mod FlagRound.
				seg.Fileoff = uint64(Rnd(int64(prev.Fileoff+prev.Filelen), int64(*FlagRound)))
				if seg.Vaddr%uint64(*FlagRound) != seg.Fileoff%uint64(*FlagRound) {
					Exitf("bad segment rounding (Vaddr=%#x Fileoff=%#x FlagRound=%#x)", seg.Vaddr, seg.Fileoff, *FlagRound)
				}
			case objabi.Hwindows:
				seg.Fileoff = prev.Fileoff + uint64(Rnd(int64(prev.Filelen), PEFILEALIGN))
			case objabi.Hplan9:
				seg.Fileoff = prev.Fileoff + prev.Filelen
			}
		}
		if seg != &Segdata {
			// Link.address already set Segdata.Filelen to
			// account for BSS.
			seg.Filelen = seg.Length
		}
		prev = seg
	}
	return prev.Fileoff + prev.Filelen
}

// add a trampoline with symbol s (to be laid down after the current function)
func (ctxt *Link) AddTramp(s *loader.SymbolBuilder) {
	s.SetType(sym.STEXT)
	s.SetReachable(true)
	s.SetOnList(true)
	ctxt.tramps = append(ctxt.tramps, s.Sym())
	if *FlagDebugTramp > 0 && ctxt.Debugvlog > 0 {
		ctxt.Logf("trampoline %s inserted\n", s.Name())
	}
}

// compressSyms compresses syms and returns the contents of the
// compressed section. If the section would get larger, it returns nil.
func compressSyms(ctxt *Link, syms []*sym.Symbol) []byte {
	var total int64
	for _, sym := range syms {
		total += sym.Size
	}

	var buf bytes.Buffer
	buf.Write([]byte("ZLIB"))
	var sizeBytes [8]byte
	binary.BigEndian.PutUint64(sizeBytes[:], uint64(total))
	buf.Write(sizeBytes[:])

	var relocbuf []byte // temporary buffer for applying relocations

	// Using zlib.BestSpeed achieves very nearly the same
	// compression levels of zlib.DefaultCompression, but takes
	// substantially less time. This is important because DWARF
	// compression can be a significant fraction of link time.
	z, err := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
	if err != nil {
		log.Fatalf("NewWriterLevel failed: %s", err)
	}
	target := &ctxt.Target
	ldr := ctxt.loader
	reporter := &ctxt.ErrorReporter
	archSyms := &ctxt.ArchSyms
	for _, s := range syms {
		// s.P may be read-only. Apply relocations in a
		// temporary buffer, and immediately write it out.
		oldP := s.P
		wasReadOnly := s.Attr.ReadOnly()
		if len(s.R) != 0 && wasReadOnly {
			relocbuf = append(relocbuf[:0], s.P...)
			s.P = relocbuf
			// TODO: This function call needs to be parallelized when the loader wavefront gets here.
			s.Attr.Set(sym.AttrReadOnly, false)
		}
		relocsym(target, ldr, reporter, archSyms, s)
		if _, err := z.Write(s.P); err != nil {
			log.Fatalf("compression failed: %s", err)
		}
		for i := s.Size - int64(len(s.P)); i > 0; {
			b := zeros[:]
			if i < int64(len(b)) {
				b = b[:i]
			}
			n, err := z.Write(b)
			if err != nil {
				log.Fatalf("compression failed: %s", err)
			}
			i -= int64(n)
		}
		// Restore s.P if a temporary buffer was used. If compression
		// is not beneficial, we'll go back to use the uncompressed
		// contents, in which case we still need s.P.
		if len(s.R) != 0 && wasReadOnly {
			s.P = oldP
			s.Attr.Set(sym.AttrReadOnly, wasReadOnly)
			for i := range s.R {
				s.R[i].Done = false
			}
		}
	}
	if err := z.Close(); err != nil {
		log.Fatalf("compression failed: %s", err)
	}
	if int64(buf.Len()) >= total {
		// Compression didn't save any space.
		return nil
	}
	return buf.Bytes()
}
