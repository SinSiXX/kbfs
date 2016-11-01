// Copyright 2016 Keybase Inc. All rights reserved.
// Use of this source code is governed by a BSD
// license that can be found in the LICENSE file.

package libkbfs

import (
	"errors"
	"fmt"
	"reflect"
	"sync"

	"golang.org/x/net/context"
)

type blockGetter interface {
	getBlock(context.Context, KeyMetadata, BlockPointer, Block) error
}

type realBlockGetter struct {
	config Config
}

func (bg *realBlockGetter) getBlock(ctx context.Context, kmd KeyMetadata, blockPtr BlockPointer, block Block) error {
	bserv := bg.config.BlockServer()
	buf, blockServerHalf, err := bserv.Get(
		ctx, kmd.TlfID(), blockPtr.ID, blockPtr.BlockContext)
	if err != nil {
		// Temporary code to track down bad block
		// requests. Remove when not needed anymore.
		if _, ok := err.(BServerErrorBadRequest); ok {
			panic(fmt.Sprintf("Bad BServer request detected: err=%s, blockPtr=%s",
				err, blockPtr))
		}

		return err
	}

	crypto := bg.config.Crypto()
	if err := crypto.VerifyBlockID(buf, blockPtr.ID); err != nil {
		return err
	}

	tlfCryptKey, err := bg.config.KeyManager().
		GetTLFCryptKeyForBlockDecryption(ctx, kmd, blockPtr)
	if err != nil {
		return err
	}

	// construct the block crypt key
	blockCryptKey, err := crypto.UnmaskBlockCryptKey(
		blockServerHalf, tlfCryptKey)
	if err != nil {
		return err
	}

	var encryptedBlock EncryptedBlock
	err = bg.config.Codec().Decode(buf, &encryptedBlock)
	if err != nil {
		return err
	}

	// decrypt the block
	err = crypto.DecryptBlock(encryptedBlock, blockCryptKey, block)
	if err != nil {
		return err
	}

	block.SetEncodedSize(uint32(len(buf)))
	return nil
}

//
// Mocked test types
//

type blockReturner struct {
	val reflect.Value
	ch  chan struct{}
}

type fakeBlockGetter struct {
	mtx      sync.RWMutex
	blockMap map[BlockPointer]blockReturner
}

func newFakeBlockGetter() *fakeBlockGetter {
	return &fakeBlockGetter{
		blockMap: make(map[BlockPointer]blockReturner),
	}
}

func (bg *fakeBlockGetter) setBlockToReturn(blockPtr BlockPointer, block Block) chan<- struct{} {
	bg.mtx.Lock()
	defer bg.mtx.Unlock()
	ch := make(chan struct{})
	bg.blockMap[blockPtr] = blockReturner{
		val: reflect.ValueOf(block).Elem(),
		ch:  ch,
	}
	return ch
}

func (bg *fakeBlockGetter) getBlock(ctx context.Context, kmd KeyMetadata, blockPtr BlockPointer, block Block) error {
	bg.mtx.RLock()
	defer bg.mtx.RUnlock()
	source, ok := bg.blockMap[blockPtr]
	if !ok {
		return errors.New("Block doesn't exist in fake block map")
	}
	// Wait until the caller tells us to continue
	<-source.ch
	destVal := reflect.ValueOf(block).Elem()
	destVal.Set(source.val)
	return nil
}