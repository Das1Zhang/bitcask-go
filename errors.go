package bitcask_go

import "errors"

var (
	ErrKeyIsEmpty             = errors.New("the key is empty")
	ErrIndexUpdateFailed      = errors.New("failed to update index")
	ErrKeyNotFound            = errors.New("key not found in database")
	ErrDataFileNotFound       = errors.New("data file is not found")
	ErrDataDirectoryCorrupted = errors.New("the database directory is corrupted")
	ErrExceedMaxBatchNum      = errors.New("exceed the max batch num")
	ErrMergeIspProgress       = errors.New("merge is in progress, try agian later")
	ErrDatabaseIsUsing        = errors.New("the database is used by another process")
	ErrMergeRatioUnreached    = errors.New("the merge ratio does not reach the option")
	ErrNoEnoughSpaceForMerge  = errors.New("no enough space for merge")
)
