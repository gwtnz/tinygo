package compiler

import (
	"fmt"
	"golang.org/x/tools/go/ssa"
	"tinygo.org/x/go-llvm"
)

// For a description of the calling convention in prose, see:
// https://tinygo.org/compiler-internals/calling-convention/

// The maximum number of arguments that can be expanded from a single struct. If
// a struct contains more fields, it is passed as a struct without expanding.
const MaxFieldsPerParam = 3

// Shortcut: create a call to runtime.<fnName> with the given arguments.
func (c *Compiler) createRuntimeCall(fnName string, args []llvm.Value, name string) llvm.Value {
	runtimePkg := c.ir.Program.ImportedPackage("runtime")
	member := runtimePkg.Members[fnName]
	if member == nil {
		panic("trying to call runtime." + fnName)
	}
	fn := c.ir.GetFunction(member.(*ssa.Function))
	if fn.LLVMFn.IsNil() {
		panic(fmt.Errorf("function %s does not appear in LLVM IR", fnName))
	}
	if !fn.IsExported() {
		args = append(args, llvm.Undef(c.i8ptrType))            // unused context parameter
		args = append(args, llvm.ConstPointerNull(c.i8ptrType)) // coroutine handle
	}
	return c.createCall(fn.LLVMFn, args, name)
}

// createCall creates a new call to runtime.<fnName> with the given arguments.
func (b *builder) createRuntimeCall(fnName string, args []llvm.Value, name string) llvm.Value {
	fullName := "runtime." + fnName
	fn := b.mod.NamedFunction(fullName)
	if fn.IsNil() {
		panic("trying to call non-existing function: " + fullName)
	}
	args = append(args, llvm.Undef(b.i8ptrType))            // unused context parameter
	args = append(args, llvm.ConstPointerNull(b.i8ptrType)) // coroutine handle
	return b.createCall(fn, args, name)
}

// Create a call to the given function with the arguments possibly expanded.
func (c *Compiler) createCall(fn llvm.Value, args []llvm.Value, name string) llvm.Value {
	expanded := make([]llvm.Value, 0, len(args))
	for _, arg := range args {
		fragments := c.expandFormalParam(arg)
		expanded = append(expanded, fragments...)
	}
	return c.builder.CreateCall(fn, expanded, name)
}

// createCall creates a call to the given function with the arguments possibly
// expanded.
func (b *builder) createCall(fn llvm.Value, args []llvm.Value, name string) llvm.Value {
	expanded := make([]llvm.Value, 0, len(args))
	for _, arg := range args {
		fragments := b.expandFormalParam(arg)
		expanded = append(expanded, fragments...)
	}
	return b.CreateCall(fn, expanded, name)
}

// Expand an argument type to a list that can be used in a function call
// parameter list.
func expandFormalParamType(t llvm.Type) []llvm.Type {
	switch t.TypeKind() {
	case llvm.StructTypeKind:
		fields := flattenAggregateType(t)
		if len(fields) <= MaxFieldsPerParam {
			return fields
		} else {
			// failed to lower
			return []llvm.Type{t}
		}
	default:
		// TODO: split small arrays
		return []llvm.Type{t}
	}
}

// Expand an argument type to a list of offsets from the start of the object.
// Used together with expandFormalParam to get the offset of each value from the
// start of the non-expanded value.
func (c *Compiler) expandFormalParamOffsets(t llvm.Type) []uint64 {
	switch t.TypeKind() {
	case llvm.StructTypeKind:
		fields := c.flattenAggregateTypeOffsets(t)
		if len(fields) <= MaxFieldsPerParam {
			return fields
		} else {
			// failed to lower
			return []uint64{0}
		}
	default:
		// TODO: split small arrays
		return []uint64{0}
	}
}

// Equivalent of expandFormalParamType for parameter values.
func (c *Compiler) expandFormalParam(v llvm.Value) []llvm.Value {
	switch v.Type().TypeKind() {
	case llvm.StructTypeKind:
		fieldTypes := flattenAggregateType(v.Type())
		if len(fieldTypes) <= MaxFieldsPerParam {
			fields := c.flattenAggregate(v)
			if len(fields) != len(fieldTypes) {
				panic("type and value param lowering don't match")
			}
			return fields
		} else {
			// failed to lower
			return []llvm.Value{v}
		}
	default:
		// TODO: split small arrays
		return []llvm.Value{v}
	}
}

// expandFormalParam splits a formal param value into pieces, so it can be
// passed directly as part of a function call. For example, it splits up small
// structs into individual fields. It is the equivalent of expandFormalParamType
// for parameter values.
func (b *builder) expandFormalParam(v llvm.Value) []llvm.Value {
	switch v.Type().TypeKind() {
	case llvm.StructTypeKind:
		fieldTypes := flattenAggregateType(v.Type())
		if len(fieldTypes) <= MaxFieldsPerParam {
			fields := b.flattenAggregate(v)
			if len(fields) != len(fieldTypes) {
				panic("type and value param lowering don't match")
			}
			return fields
		} else {
			// failed to lower
			return []llvm.Value{v}
		}
	default:
		// TODO: split small arrays
		return []llvm.Value{v}
	}
}

// Try to flatten a struct type to a list of types. Returns a 1-element slice
// with the passed in type if this is not possible.
func flattenAggregateType(t llvm.Type) []llvm.Type {
	switch t.TypeKind() {
	case llvm.StructTypeKind:
		fields := make([]llvm.Type, 0, t.StructElementTypesCount())
		for _, subfield := range t.StructElementTypes() {
			subfields := flattenAggregateType(subfield)
			fields = append(fields, subfields...)
		}
		return fields
	default:
		return []llvm.Type{t}
	}
}

// Return the offsets from the start of the object if this object type were
// flattened like in flattenAggregate. Used together with flattenAggregate to
// know the start indices of each value in the non-flattened object.
func (c *Compiler) flattenAggregateTypeOffsets(t llvm.Type) []uint64 {
	switch t.TypeKind() {
	case llvm.StructTypeKind:
		fields := make([]uint64, 0, t.StructElementTypesCount())
		for fieldIndex, field := range t.StructElementTypes() {
			suboffsets := c.flattenAggregateTypeOffsets(field)
			offset := c.targetData.ElementOffset(t, fieldIndex)
			for i := range suboffsets {
				suboffsets[i] += offset
			}
			fields = append(fields, suboffsets...)
		}
		return fields
	default:
		return []uint64{0}
	}
}

// Break down a struct into its elementary types for argument passing. The value
// equivalent of flattenAggregateType
func (c *Compiler) flattenAggregate(v llvm.Value) []llvm.Value {
	switch v.Type().TypeKind() {
	case llvm.StructTypeKind:
		fields := make([]llvm.Value, 0, v.Type().StructElementTypesCount())
		for i := range v.Type().StructElementTypes() {
			subfield := c.builder.CreateExtractValue(v, i, "")
			subfields := c.flattenAggregate(subfield)
			fields = append(fields, subfields...)
		}
		return fields
	default:
		return []llvm.Value{v}
	}
}

// flattenAggregate breaks down a struct into its elementary values for argument
// passing. It is the value equivalent of flattenAggregateType
func (b *builder) flattenAggregate(v llvm.Value) []llvm.Value {
	switch v.Type().TypeKind() {
	case llvm.StructTypeKind:
		fields := make([]llvm.Value, 0, v.Type().StructElementTypesCount())
		for i := range v.Type().StructElementTypes() {
			subfield := b.CreateExtractValue(v, i, "")
			subfields := b.flattenAggregate(subfield)
			fields = append(fields, subfields...)
		}
		return fields
	default:
		return []llvm.Value{v}
	}
}

// Collapse a list of fields into its original value.
func (c *Compiler) collapseFormalParam(t llvm.Type, fields []llvm.Value) llvm.Value {
	param, remaining := c.collapseFormalParamInternal(t, fields)
	if len(remaining) != 0 {
		panic("failed to expand back all fields")
	}
	return param
}

// Returns (value, remainingFields). Used by collapseFormalParam.
func (c *Compiler) collapseFormalParamInternal(t llvm.Type, fields []llvm.Value) (llvm.Value, []llvm.Value) {
	switch t.TypeKind() {
	case llvm.StructTypeKind:
		if len(flattenAggregateType(t)) <= MaxFieldsPerParam {
			value := llvm.ConstNull(t)
			for i, subtyp := range t.StructElementTypes() {
				structField, remaining := c.collapseFormalParamInternal(subtyp, fields)
				fields = remaining
				value = c.builder.CreateInsertValue(value, structField, i, "")
			}
			return value, fields
		} else {
			// this struct was not flattened
			return fields[0], fields[1:]
		}
	default:
		return fields[0], fields[1:]
	}
}
