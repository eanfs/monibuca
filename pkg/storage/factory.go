package storage

var Factory = map[string]func(any) (Storage, error){}
