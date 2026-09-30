package redis

import (
	"fmt"
	"strconv"

	"github.com/truongpx396/intel-payment/metering/domain"
)

// reply is a hot function's array reply: {status, ...}.
type reply []any

func asReply(v any) (reply, error) {
	r, ok := v.([]any)
	if !ok || len(r) == 0 {
		return nil, fmt.Errorf("redis: malformed hot function reply %#v", v)
	}
	return r, nil
}

func (r reply) status() string {
	s, _ := r[0].(string)
	return s
}

// num reads element i as an integer. Lua numbers arrive as integer replies; a value the function
// formatted as a string is accepted too.
func (r reply) num(i int) (int64, error) {
	if i >= len(r) {
		return 0, fmt.Errorf("redis: reply has no element %d: %#v", i, []any(r))
	}
	switch v := r[i].(type) {
	case int64:
		return v, nil
	case string:
		return strconv.ParseInt(v, 10, 64)
	}
	return 0, fmt.Errorf("redis: reply element %d is %T, not a number", i, r[i])
}

// pools reads a flat {pool, balance, pool, balance…} array into a map.
func poolsOf(v any) (map[domain.Pool]domain.Credits, error) {
	flat, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("redis: pools element is %T", v)
	}
	out := make(map[domain.Pool]domain.Credits, len(flat)/2)
	for i := 0; i+1 < len(flat); i += 2 {
		name, ok := flat[i].(string)
		if !ok {
			return nil, fmt.Errorf("redis: pool name is %T", flat[i])
		}
		n, err := toInt(flat[i+1])
		if err != nil {
			return nil, err
		}
		out[domain.Pool(name)] = domain.Credits(n)
	}
	return out, nil
}

func toInt(v any) (int64, error) {
	switch n := v.(type) {
	case int64:
		return n, nil
	case string:
		return strconv.ParseInt(n, 10, 64)
	}
	return 0, fmt.Errorf("redis: %T is not a number", v)
}
