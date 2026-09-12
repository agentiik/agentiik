package local

import (
	"github.com/agentiik/agentiik/artifact"
)

// store opens the artifact store of one namespace over the working directory.
//
// artifact.Dir is the byte layer and the namespace is the store's own, so every physical
// key is <namespace>/sha256/<digest>: two runs over the same inputs write the same object
// once, which is content addressing proving itself rather than being described, and two
// namespaces cannot share an object even on a laptop where there is only ever one.
//
// The store is kept per namespace for as long as the session lives, because a Store is a
// handle over a directory and opening a second one for the same namespace would be a
// second answer to one question.
func (s *Session) store(namespace string) (*artifact.Store, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if store, ok := s.stores[namespace]; ok {
		return store, nil
	}
	store, err := artifact.New(artifact.Dir(s.layout.Objects()), namespace, s.limits)
	if err != nil {
		return nil, err
	}
	s.stores[namespace] = store
	return store, nil
}
