package fileSystem

import (
	"fmt"
	"path"
	"sort"
	"sync"
	"time"

	etcdErr "github.com/coreos/etcd/error"
)

var (
	Permanent time.Time
)

const (
	normal = iota
	removed
)

type Node struct {
	Path          string
	CreateIndex   uint64
	CreateTerm    uint64
	ModifiedIndex uint64
	ModifiedTerm  uint64
	Parent        *Node
	ExpireTime    time.Time
	ACL           string
	Value         string           // for key-value pair
	Children      map[string]*Node // for directory
	status        int
	mu            sync.Mutex
	stopExpire    chan bool // stop expire routine channel
}

func newFile(nodePath string, value string, createIndex uint64, createTerm uint64, parent *Node, ACL string, expireTime time.Time) *Node {
	return &Node{
		Path:          nodePath,
		CreateIndex:   createIndex,
		CreateTerm:    createTerm,
		ModifiedIndex: createIndex,
		ModifiedTerm:  createTerm,
		Parent:        parent,
		ACL:           ACL,
		stopExpire:    make(chan bool, 1),
		ExpireTime:    expireTime,
		Value:         value,
	}
}

func newDir(nodePath string, createIndex uint64, createTerm uint64, parent *Node, ACL string, expireTime time.Time) *Node {
	return &Node{
		Path:        nodePath,
		CreateIndex: createIndex,
		CreateTerm:  createTerm,
		Parent:      parent,
		ACL:         ACL,
		stopExpire:  make(chan bool, 1),
		ExpireTime:  expireTime,
		Children:    make(map[string]*Node),
	}
}

// Remove function remove the node.
// If the node is a directory and recursive is true, the function will recursively remove
// add nodes under the receiver node.
func (n *Node) Remove(recursive bool, callback func(path string)) error {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.status == removed {
		return nil
	}

	if !n.IsDir() { // file node: key-value pair
		_, name := path.Split(n.Path)

		if n.Parent.Children[name] == n {
			// This is the only pointer to Node object
			// Handled by garbage collector
			delete(n.Parent.Children, name)

			if callback != nil {
				callback(n.Path)
			}

			n.stopExpire <- true
			n.status = removed
		}

		return nil
	}

	if !recursive {
		return etcdErr.NewError(102, "")
	}

	for _, child := range n.Children { // delete all children
		child.Remove(true, callback)
	}

	// delete self
	_, name := path.Split(n.Path)
	if n.Parent.Children[name] == n {
		delete(n.Parent.Children, name)

		if callback != nil {
			callback(n.Path)
		}

		n.stopExpire <- true
		n.status = removed
	}

	return nil
}

// Get function gets the value of the node.
// If the receiver node is not a key-value pair, a "Not A File" error will be returned.
func (n *Node) Read() (string, error) {
	if n.IsDir() {
		return "", etcdErr.NewError(102, "")
	}

	return n.Value, nil
}

// Set function set the value of the node to the given value.
// If the receiver node is a directory, a "Not A File" error will be returned.
func (n *Node) Write(value string, index uint64, term uint64) error {
	if n.IsDir() {
		return etcdErr.NewError(102, "")
	}

	n.Value = value
	n.ModifiedIndex = index
	n.ModifiedTerm = term

	return nil
}

// List function return a slice of nodes under the receiver node.
// If the receiver node is not a directory, a "Not A Directory" error will be returned.
func (n *Node) List() ([]*Node, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.IsDir() {
		return nil, etcdErr.NewError(104, "")
	}

	nodes := make([]*Node, len(n.Children))

	i := 0
	for _, node := range n.Children {
		nodes[i] = node
		i++
	}

	return nodes, nil
}

func (n *Node) GetFile(name string) (*Node, error) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if !n.IsDir() {
		return nil, etcdErr.NewError(104, n.Path)
	}

	f, ok := n.Children[name]

	if ok {
		if !f.IsDir() {
			return f, nil
		} else {
			return nil, etcdErr.NewError(102, f.Path)
		}
	}

	return nil, nil

}

// Add function adds a node to the receiver node.
// If the receiver is not a directory, a "Not A Directory" error will be returned.
// If there is a existing node with the same name under the directory, a "Already Exist"
// error will be returned
func (n *Node) Add(child *Node) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.status == removed {
		return etcdErr.NewError(100, "")
	}

	if !n.IsDir() {
		return etcdErr.NewError(104, "")
	}

	_, name := path.Split(child.Path)

	_, ok := n.Children[name]

	if ok {
		return etcdErr.NewError(105, "")
	}

	n.Children[name] = child

	return nil

}

// Clone function clone the node recursively and return the new node.
// If the node is a directory, it will clone all the content under this directory.
// If the node is a key-value pair, it will clone the pair.
func (n *Node) Clone() *Node {
	n.mu.Lock()
	defer n.mu.Unlock()
	if !n.IsDir() {
		return newFile(n.Path, n.Value, n.CreateIndex, n.CreateTerm, n.Parent, n.ACL, n.ExpireTime)
	}

	clone := newDir(n.Path, n.CreateIndex, n.CreateTerm, n.Parent, n.ACL, n.ExpireTime)

	for key, child := range n.Children {
		clone.Children[key] = child.Clone()
	}

	return clone
}

// IsDir function checks whether the node is a directory.
// If the node is a directory, the function will return true.
// Otherwise the function will return false.
func (n *Node) IsDir() bool {

	if n.Children == nil { // key-value pair
		return false
	}

	return true
}

func (n *Node) Expire() {
	duration := n.ExpireTime.Sub(time.Now())
	if duration <= 0 {
		n.Remove(true, nil)
		return
	}

	select {
	// if timeout, delete the node
	case <-time.After(duration):
		n.Remove(true, nil)
		return

	// if stopped, return
	case <-n.stopExpire:
		fmt.Println("expire stopped")
		return

	}
}

// IsHidden function checks if the node is a hidden node. A hidden node
// will begin with '_'

// A hidden node will not be shown via get command under a directory
// For example if we have /foo/_hidden and /foo/notHidden, get "/foo"
// will only return /foo/notHidden
func (n *Node) IsHidden() bool {
	_, name := path.Split(n.Path)

	if name[0] == '_' { //hidden
		return true
	}

	return false
}

func (n *Node) Pair(recurisive, sorted bool) KeyValuePair {

	if n.IsDir() {
		pair := KeyValuePair{
			Key: n.Path,
			Dir: true,
		}
		if !recurisive {
			return pair
		}

		children, _ := n.List()
		pair.KVPairs = make([]KeyValuePair, len(children))

		// we do not use the index in the children slice directly
		// we need to skip the hidden one
		i := 0

		for _, child := range children {

			if child.IsHidden() { // get will not list hidden node
				continue
			}

			pair.KVPairs[i] = child.Pair(recurisive, sorted)

			i++
		}

		// eliminate hidden nodes
		pair.KVPairs = pair.KVPairs[:i]
		if sorted {
			sort.Sort(pair)
		}
		return pair
	}

	return KeyValuePair{
		Key:   n.Path,
		Value: n.Value,
	}
}
