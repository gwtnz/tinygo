package compiler

// This file emits the correct map intrinsics for map operations.

import (
	"go/token"
	"go/types"

	"tinygo.org/x/go-llvm"
)

// createMapLookup returns the value in a map. It calls a runtime function
// depending on the map key type to load the map value and its comma-ok value.
func (b *builder) createMapLookup(keyType, valueType types.Type, m, key llvm.Value, commaOk bool, pos token.Pos) (llvm.Value, error) {
	llvmValueType := b.getLLVMType(valueType)

	// Allocate the memory for the resulting type. Do not zero this memory: it
	// will be zeroed by the hashmap get implementation if the key is not
	// present in the map.
	mapValueAlloca, mapValuePtr, mapValueSize := b.createTemporaryAlloca(llvmValueType, "hashmap.value")

	// Do the lookup. How it is done depends on the key type.
	var commaOkValue llvm.Value
	if t, ok := keyType.(*types.Basic); ok && t.Info()&types.IsString != 0 {
		// key is a string
		params := []llvm.Value{m, key, mapValuePtr}
		commaOkValue = b.createRuntimeCall("hashmapStringGet", params, "")
	} else if hashmapIsBinaryKey(keyType) {
		// key can be compared with runtime.memequal
		// Store the key in an alloca, in the entry block to avoid dynamic stack
		// growth.
		mapKeyAlloca, mapKeyPtr, mapKeySize := b.createTemporaryAlloca(key.Type(), "hashmap.key")
		b.CreateStore(key, mapKeyAlloca)
		// Fetch the value from the hashmap.
		params := []llvm.Value{m, mapKeyPtr, mapValuePtr}
		commaOkValue = b.createRuntimeCall("hashmapBinaryGet", params, "")
		b.emitLifetimeEnd(mapKeyPtr, mapKeySize)
	} else {
		// Not trivially comparable using memcmp.
		return llvm.Value{}, b.makeError(pos, "only strings, bools, ints, pointers or structs of bools/ints are supported as map keys, but got: "+keyType.String())
	}

	// Load the resulting value from the hashmap. The value is set to the zero
	// value if the key doesn't exist in the hashmap.
	mapValue := b.CreateLoad(mapValueAlloca, "")
	b.emitLifetimeEnd(mapValuePtr, mapValueSize)

	if commaOk {
		tuple := llvm.Undef(b.ctx.StructType([]llvm.Type{llvmValueType, b.ctx.Int1Type()}, false))
		tuple = b.CreateInsertValue(tuple, mapValue, 0, "")
		tuple = b.CreateInsertValue(tuple, commaOkValue, 1, "")
		return tuple, nil
	} else {
		return mapValue, nil
	}
}

// createMapUpdate updates a map key to a given value, by creating an
// appropriate runtime call.
func (b *builder) createMapUpdate(keyType types.Type, m, key, value llvm.Value, pos token.Pos) {
	valueAlloca, valuePtr, valueSize := b.createTemporaryAlloca(value.Type(), "hashmap.value")
	b.CreateStore(value, valueAlloca)
	keyType = keyType.Underlying()
	if t, ok := keyType.(*types.Basic); ok && t.Info()&types.IsString != 0 {
		// key is a string
		params := []llvm.Value{m, key, valuePtr}
		b.createRuntimeCall("hashmapStringSet", params, "")
	} else if hashmapIsBinaryKey(keyType) {
		// key can be compared with runtime.memequal
		keyAlloca, keyPtr, keySize := b.createTemporaryAlloca(key.Type(), "hashmap.key")
		b.CreateStore(key, keyAlloca)
		params := []llvm.Value{m, keyPtr, valuePtr}
		b.createRuntimeCall("hashmapBinarySet", params, "")
		b.emitLifetimeEnd(keyPtr, keySize)
	} else {
		b.addError(pos, "only strings, bools, ints, pointers or structs of bools/ints are supported as map keys, but got: "+keyType.String())
	}
	b.emitLifetimeEnd(valuePtr, valueSize)
}

// createMapDelete deletes a key from a map by calling the appropriate runtime
// function. It is the implementation of the Go delete() builtin.
func (b *builder) createMapDelete(keyType types.Type, m, key llvm.Value, pos token.Pos) error {
	keyType = keyType.Underlying()
	if t, ok := keyType.(*types.Basic); ok && t.Info()&types.IsString != 0 {
		// key is a string
		params := []llvm.Value{m, key}
		b.createRuntimeCall("hashmapStringDelete", params, "")
		return nil
	} else if hashmapIsBinaryKey(keyType) {
		keyAlloca, keyPtr, keySize := b.createTemporaryAlloca(key.Type(), "hashmap.key")
		b.CreateStore(key, keyAlloca)
		params := []llvm.Value{m, keyPtr}
		b.createRuntimeCall("hashmapBinaryDelete", params, "")
		b.emitLifetimeEnd(keyPtr, keySize)
		return nil
	} else {
		return b.makeError(pos, "only strings, bools, ints, pointers or structs of bools/ints are supported as map keys, but got: "+keyType.String())
	}
}

// Get FNV-1a hash of this string.
//
// https://en.wikipedia.org/wiki/Fowler%E2%80%93Noll%E2%80%93Vo_hash_function#FNV-1a_hash
func hashmapHash(data []byte) uint32 {
	var result uint32 = 2166136261 // FNV offset basis
	for _, c := range data {
		result ^= uint32(c)
		result *= 16777619 // FNV prime
	}
	return result
}

// Get the topmost 8 bits of the hash, without using a special value (like 0).
func hashmapTopHash(hash uint32) uint8 {
	tophash := uint8(hash >> 24)
	if tophash < 1 {
		// 0 means empty slot, so make it bigger.
		tophash += 1
	}
	return tophash
}

// Returns true if this key type does not contain strings, interfaces etc., so
// can be compared with runtime.memequal.
func hashmapIsBinaryKey(keyType types.Type) bool {
	switch keyType := keyType.(type) {
	case *types.Basic:
		return keyType.Info()&(types.IsBoolean|types.IsInteger) != 0
	case *types.Pointer:
		return true
	case *types.Struct:
		for i := 0; i < keyType.NumFields(); i++ {
			fieldType := keyType.Field(i).Type().Underlying()
			if !hashmapIsBinaryKey(fieldType) {
				return false
			}
		}
		return true
	case *types.Array:
		return hashmapIsBinaryKey(keyType.Elem())
	case *types.Named:
		return hashmapIsBinaryKey(keyType.Underlying())
	default:
		return false
	}
}
