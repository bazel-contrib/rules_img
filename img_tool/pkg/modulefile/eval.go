package modulefile

// Value is the result of evaluating an expression: a string, an int64, a
// bool, nil (None), a []Value (list or tuple), a *DictValue, or Unknown.
type Value any

// Unknown is the value of an expression that can't be evaluated statically,
// such as a call, a comprehension or an unbound name.
type Unknown struct{}

// DictValue is an evaluated dict, keeping the order of its entries.
type DictValue struct {
	Keys   []Value
	Values []Value
}

// Env binds top-level names to values.
type Env map[string]Value

// Eval evaluates x in env. Only literals, names bound in env, and + on
// strings and lists are evaluated; anything else is Unknown.
func Eval(x Expr, env Env) Value {
	switch x := x.(type) {
	case *String:
		return x.Value
	case *Number:
		var n int64
		for _, c := range x.Raw {
			if c < '0' || c > '9' {
				return Unknown{}
			}
			n = n*10 + int64(c-'0')
		}
		return n
	case *Ident:
		switch x.Name {
		case "True":
			return true
		case "False":
			return false
		case "None":
			return nil
		}
		if value, ok := env[x.Name]; ok {
			return value
		}
		return Unknown{}
	case *List:
		return evalElems(x.Elems, env)
	case *Tuple:
		return evalElems(x.Elems, env)
	case *Dict:
		dict := &DictValue{}
		for _, entry := range x.Entries {
			dict.Keys = append(dict.Keys, Eval(entry.Key, env))
			dict.Values = append(dict.Values, Eval(entry.Value, env))
		}
		return dict
	case *Unary:
		value := Eval(x.X, env)
		switch x.Op {
		case "-":
			if n, ok := value.(int64); ok {
				return -n
			}
		case "+":
			if n, ok := value.(int64); ok {
				return n
			}
		case "not":
			if b, ok := value.(bool); ok {
				return !b
			}
		}
		return Unknown{}
	case *Binary:
		if x.Op != "+" {
			return Unknown{}
		}
		left, right := Eval(x.X, env), Eval(x.Y, env)
		switch left := left.(type) {
		case string:
			if right, ok := right.(string); ok {
				return left + right
			}
		case int64:
			if right, ok := right.(int64); ok {
				return left + right
			}
		case []Value:
			if right, ok := right.([]Value); ok {
				return append(append([]Value{}, left...), right...)
			}
		}
		return Unknown{}
	}
	return Unknown{}
}

func evalElems(elems []Expr, env Env) Value {
	values := make([]Value, 0, len(elems))
	for _, elem := range elems {
		values = append(values, Eval(elem, env))
	}
	return values
}

// IsLiteral reports whether x is written out in place, rather than computed or
// taken from a variable: a string, number, True/False/None, or a list, tuple
// or dict of literals. Only literal attribute values can be rewritten in place.
func IsLiteral(x Expr) bool {
	switch x := x.(type) {
	case *String, *Number:
		return true
	case *Ident:
		return x.Name == "True" || x.Name == "False" || x.Name == "None"
	case *Unary:
		return x.Op == "-" && IsLiteral(x.X)
	case *List:
		return allLiteral(x.Elems)
	case *Tuple:
		return allLiteral(x.Elems)
	case *Dict:
		for _, entry := range x.Entries {
			if !IsLiteral(entry.Key) || !IsLiteral(entry.Value) {
				return false
			}
		}
		return true
	}
	return false
}

func allLiteral(elems []Expr) bool {
	for _, elem := range elems {
		if !IsLiteral(elem) {
			return false
		}
	}
	return true
}

// AsString returns value as a string, if it is one.
func AsString(value Value) (string, bool) {
	s, ok := value.(string)
	return s, ok
}

// AsStrings returns value as a list of strings, if it is a list (or tuple) of
// strings.
func AsStrings(value Value) ([]string, bool) {
	list, ok := value.([]Value)
	if !ok {
		return nil, false
	}
	strs := make([]string, 0, len(list))
	for _, elem := range list {
		s, ok := elem.(string)
		if !ok {
			return nil, false
		}
		strs = append(strs, s)
	}
	return strs, true
}

// AsBool returns value as a bool, if it is one.
func AsBool(value Value) (bool, bool) {
	b, ok := value.(bool)
	return b, ok
}

// Bind evaluates the assignment a in env and records the result. Augmented
// assignments are evaluated as x = x <op> value; tuple assignments bind every
// target to Unknown unless the value is a list of the same length.
func (a *Assign) Bind(env Env) {
	value := Eval(a.Value, env)
	if a.Op != "=" {
		if len(a.Targets) != 1 {
			return
		}
		op := a.Op[:len(a.Op)-1]
		value = Eval(&Binary{Op: op, X: &Ident{Name: a.Targets[0]}, Y: a.Value}, env)
	}
	if len(a.Targets) == 1 {
		env[a.Targets[0]] = value
		return
	}
	values, ok := value.([]Value)
	for i, target := range a.Targets {
		if ok && len(values) == len(a.Targets) {
			env[target] = values[i]
		} else {
			env[target] = Unknown{}
		}
	}
}
