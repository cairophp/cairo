package spec

import "fmt"

// Claims holds what the declared servers must not share: a listen address
// belongs to one server, and a PHP handler id to one route or service. Every
// extension that declares servers checks its own against the same Claims.
// The zero value is ready to use.
type Claims struct {
	listens map[string]bool
	ids     map[int]bool
}

// Listen claims a server's listen address.
func (c *Claims) Listen(addr string) error {
	if addr == "" {
		return fmt.Errorf("a server has an empty listen address")
	}
	if c.listens[addr] {
		return fmt.Errorf("two servers listen on %s", addr)
	}
	if c.listens == nil {
		c.listens = map[string]bool{}
	}
	c.listens[addr] = true
	return nil
}

// Handler claims the id of the PHP handler of a route or a service.
func (c *Claims) Handler(id int) error {
	if c.ids[id] {
		return fmt.Errorf("duplicate handler id %d", id)
	}
	if c.ids == nil {
		c.ids = map[int]bool{}
	}
	c.ids[id] = true
	return nil
}
