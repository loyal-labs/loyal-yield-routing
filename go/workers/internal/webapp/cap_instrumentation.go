package webapp

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"encoding/json"
	"math/rand/v2"
	"regexp"
	"strconv"
	"strings"
)

func generateCapInstrumentation(expires int64) (string, capInstrumentMeta, error) {
	id := randomHex(16)
	vars := make([]string, 4)
	for i := range vars {
		vars[i] = capVar(12)
	}
	var initVals, vals [4]int32
	for i := range initVals {
		initVals[i] = int32(capRand(10, 250))
	}
	vals = initVals
	correctKey := capRand(1000, 9000)
	badKey := capRand(1000, 9000)
	for badKey == correctKey {
		badKey = capRand(1000, 9000)
	}
	vals[0] ^= int32(correctKey)

	fnHelper, domHelper := capVar(0), capVar(0)
	var eqs strings.Builder
	eqs.WriteString(`function ` + fnHelper + `(a,b,c){function F(d){this.v=function(){return this.k^d;}};var p={k:c};var i=new F(a);i.k=b;F.prototype=p;return i.v()|(new F(b)).v();}` +
		`function ` + domHelper + `(x,y,z){var d=document.createElement('div');d.style.display='none';document.body.appendChild(d);function A(p,v){for(var i=0;i<8;i++){var c=document.createElement('div');p.appendChild(c);c.innerText=v;if((v&1)==0)p=c;v=v>>1;}return p;}function B(n,r,s){if(!n||n==r)return s%256;while(n.children.length>0)n.removeChild(n.lastElementChild);return B(n.parentNode,r,s+parseInt(n.innerText));}var s=B(A(A(A(d,x),y),z),d,0);d.parentNode.removeChild(d);return s;}`)
	eqs.WriteString(vars[0] + ` = ` + vars[0] + ` ^ (navigator.userAgent ? ` + itoa(correctKey) + ` : ` + itoa(badKey) + `);`)
	for range 20 {
		op, dest, src1, src2, src3 := capRand(0, 5), capRand(0, 3), capRand(0, 3), capRand(0, 3), capRand(0, 3)
		vD, vS1, vS2, vS3 := vars[dest], vars[src1], vars[src2], vars[src3]
		switch op {
		case 0:
			eqs.WriteString(vD + ` = ~(` + vD + ` & ` + vS1 + `);`)
			vals[dest] = ^(vals[dest] & vals[src1])
		case 1:
			eqs.WriteString(vD + ` = ` + vD + ` ^ ` + vS1 + `;`)
			vals[dest] ^= vals[src1]
		case 2:
			eqs.WriteString(vD + ` = ` + vD + ` | ` + vS1 + `;`)
			vals[dest] |= vals[src1]
		case 3:
			eqs.WriteString(vD + ` = ` + vD + ` & ` + vS1 + `;`)
			vals[dest] &= vals[src1]
		case 4:
			eqs.WriteString(vD + ` = ` + fnHelper + `(` + vS1 + `, ` + vS2 + `, ` + vD + `);`)
			vals[dest] = (vals[src2] ^ vals[src1]) | (vals[dest] ^ vals[src2])
		default:
			eqs.WriteString(vD + ` = ` + domHelper + `(` + vS1 + `, ` + vS2 + `, ` + vS3 + `);`)
			vals[dest] = capDOMSum(vals[src1], vals[src2], vals[src3])
		}
	}
	for i := range vars {
		salt := capRand(100_000, 999_999)
		eqs.WriteString(vars[i] + `=((` + vars[i] + `^` + itoa(salt) + `)&0x7FFFFFFF)%900000+100000;`)
		vals[i] = ((vals[i]^int32(salt))&0x7fffffff)%900_000 + 100_000
	}

	script := capWhitespace.ReplaceAllString(capClientScript(id, vars, initVals, eqs.String()), "\n")
	var compressed bytes.Buffer
	writer, err := flate.NewWriter(&compressed, 1)
	if err != nil {
		return "", capInstrumentMeta{}, err
	}
	if _, err := writer.Write([]byte(script)); err != nil {
		return "", capInstrumentMeta{}, err
	}
	if err := writer.Close(); err != nil {
		return "", capInstrumentMeta{}, err
	}
	return base64.StdEncoding.EncodeToString(compressed.Bytes()), capInstrumentMeta{
		ID: id, ExpectedVals: vals[:], Vars: vars, BlockAutomatedBrowsers: false, Expires: expires,
	}, nil
}

var capWhitespace = regexp.MustCompile(`\n\s*`)

func capDOMSum(x, y, z int32) int32 {
	type node struct {
		parent *node
		value  int32
	}
	root := &node{}
	chain := func(parent *node, v int32) *node {
		current := parent
		for range 8 {
			child := &node{parent: current, value: v}
			if v&1 == 0 {
				current = child
			}
			v >>= 1
		}
		return current
	}
	var sum int64
	for n := chain(chain(chain(root, x), y), z); n != root; n = n.parent {
		sum += int64(n.value)
	}
	return int32(sum % 256)
}

func capClientScript(id string, vars []string, initVals [4]int32, clientEqs string) string {
	seed := uint32(rand.IntN(0x7fffffff-1) + 1)
	hash := func(s string) uint32 { return fnv1a(seed, s) }
	hashes := func(names ...string) string {
		out := make([]uint32, len(names))
		for i, name := range names {
			out[i] = hash(name)
		}
		encoded, _ := json.Marshal(out)
		return string(encoded)
	}
	hF, hSet := capVar(0), capVar(0)
	evalLocal := capVar(0)
	evalSecret := rand.IntN(0x7fffffff-1_000_000) + 1_000_000
	evalA, evalB, evalC := capVar(0), capVar(0), capVar(0)
	n := capVar(0)
	r := strings.NewReplacer(
		"@n@", n, "@hF@", hF, "@hSet@", hSet,
		"@hNav@", u32(hash("[object Navigator]")), "@hWin@", u32(hash("[object Window]")), "@hDoc@", u32(hash("[object HTMLDocument]")),
		"@leak1@", hashes("Bun", "process", "module", "require", "global", "__dirname", "Deno"),
		"@leak2@", hashes("Bun", "process", "require", "global", "__dirname", "Deno"),
		"@eL@", evalLocal, "@eLa@", evalLocal[:2], "@eLb@", evalLocal[2:],
		"@eS@", itoa(evalSecret), "@eS1@", itoa(evalSecret+1), "@eS2@", itoa(evalSecret*2),
		"@eA@", evalA, "@eB@", evalB, "@eC@", evalC,
	)
	checks := []string{
		`try { const @n@st = (new Error()).stack || ''; if (@n@st.indexOf('node:internal') !== -1 || @n@st.indexOf('moduleEvaluation') !== -1 || @n@st.indexOf('loadAndEvaluateModule') !== -1 || @n@st.indexOf('file:///') !== -1 || @n@st.indexOf('[eval]') !== -1 || /\(native:/.test(@n@st)) return null; } catch { return null }`,
		`if (typeof HTMLElement !== 'function' || typeof Window !== 'function' || typeof Document !== 'function' || typeof Navigator !== 'function' || typeof Node !== 'function') return null; if (!(navigator instanceof Navigator) || !(document instanceof Document) || !(window instanceof Window) || !(document.body instanceof HTMLElement)) return null; if (globalThis !== window || window.self !== window || document.defaultView !== window) return null;`,
		`try { const @n@ots = Object.prototype.toString; if (@hF@(@n@ots.call(navigator)) !== @hNav@ || @hF@(@n@ots.call(window)) !== @hWin@ || @hF@(@n@ots.call(document)) !== @hDoc@) return null; } catch { return null }`,
		`try { if (typeof EventTarget !== 'function' || !(document.body instanceof EventTarget) || !(window instanceof EventTarget)) return null; const @n@probe = document.createElement('div'); let @n@fired = 0; const @n@ev = '_c' + (Date.now() & 0xffff).toString(36); const @n@h = (e) => { if (e && e.detail === 0xc0de) @n@fired++; }; @n@probe.addEventListener(@n@ev, @n@h); @n@probe.dispatchEvent(new CustomEvent(@n@ev, { detail: 0xc0de })); @n@probe.dispatchEvent(new CustomEvent(@n@ev, { detail: 0xc0de })); @n@probe.removeEventListener(@n@ev, @n@h); @n@probe.dispatchEvent(new CustomEvent(@n@ev, { detail: 0xc0de })); if (@n@fired !== 2) return null; } catch { return null }`,
		`try { const @n@gf = new Function('return this'); const @n@tg = @n@gf(); if (@n@tg !== globalThis) return null; const @n@leakHashes = @leak1@; for (const @n@k of Object.getOwnPropertyNames(@n@tg)) { if (@hSet@(@n@leakHashes, @hF@(@n@k))) return null; } const @n@fnArgs = new Function('a','b','c','return a+b+c'); if (@n@fnArgs.length !== 3) return null; if (@n@fnArgs(10,20,30) !== 60) return null; const @n@src = Function.prototype.toString.call(@n@fnArgs); if (@n@src.indexOf('return a+b+c') === -1) return null; if (@n@src.indexOf('apply') !== -1 || @n@src.indexOf('callArgs') !== -1 || @n@src.indexOf('Reflect.') !== -1) return null; let @n@thrown = null; try { new Function('throw new Error("x")')(); } catch (@n@e) { @n@thrown = @n@e; } if (!@n@thrown || !@n@thrown.stack) return null; const @n@st2 = @n@thrown.stack; if (@n@st2.indexOf('node:internal') !== -1 || @n@st2.indexOf('moduleEvaluation') !== -1 || @n@st2.indexOf('file:///') !== -1 || @n@st2.indexOf('[eval]') !== -1 || /\(native:/.test(@n@st2)) return null; } catch { return null }`,
		`try { const @n@ie = (0, eval); const @n@eg = @n@ie('this'); if (@n@eg !== globalThis) return null; const @n@leak2 = @leak2@; for (const @n@k of Object.getOwnPropertyNames(@n@eg)) { if (@hSet@(@n@leak2, @hF@(@n@k))) return null; } } catch { return null }`,
		`try { var @eL@ = @eS@; var @eA@ = '@eLa@'; var @eB@ = '@eLb@'; var @eC@ = @eA@ + @eB@; var @eL@r1 = (0, eval)('typeof ' + @eC@); if (@eL@r1 !== 'undefined') return null; var @eL@r2 = eval(@eC@); if (@eL@r2 !== @eS@) return null; var @eL@r3 = eval(@eA@ + @eB@ + '+1'); if (@eL@r3 !== @eS1@) return null; var @eL@arr = ['(', '(', ')', '=', '>', 't', 'h', 'i', 's', ')', '(', ')']; var @eL@arrow = (0, eval)(@eL@arr.join('')); if (@eL@arrow !== globalThis) return null; var @eL@r4 = eval('(function(){return ' + @eC@ + '*2;})()'); if (@eL@r4 !== @eS2@) return null; } catch { return null }`,
	}
	rand.Shuffle(len(checks), func(i, j int) { checks[i], checks[j] = checks[j], checks[i] })
	helpers := `function ` + hF + `(s){let h=` + u32(seed) + `>>>0;for(let i=0;i<s.length;i++){h^=s.charCodeAt(i);h=(h+(h<<1)+(h<<4)+(h<<7)+(h<<8)+(h<<24))>>>0;}return h>>>0;}` +
		`function ` + hSet + `(a,v){for(var i=0;i<a.length;i++)if(a[i]===v)return true;return false;}`
	dv, out := capVar(0), capVar(0)
	quotedID := strconv.Quote(id)
	return `(function(){window.onload=async function(){try {` + helpers + `const ` + dv + `=await (async function(){` + r.Replace(strings.Join(checks, "")) + "\n" +
		`      var ` + vars[0] + `=` + i32(initVals[0]) + `;var ` + vars[1] + `=` + i32(initVals[1]) + `;var ` + vars[2] + `=` + i32(initVals[2]) + `;var ` + vars[3] + `=` + i32(initVals[3]) + `;` + clientEqs + "\n" +
		`      var ` + out + `={};` + out + `["` + vars[0] + `"]=` + vars[0] + `;` + out + `["` + vars[1] + `"]=` + vars[1] + `;` + out + `["` + vars[2] + `"]=` + vars[2] + `;` + out + `["` + vars[3] + `"]=` + vars[3] + `;return ` + out + `;})();if (!` + dv + ` || typeof ` + dv + ` !== 'object') return;parent.postMessage({type: 'cap:instr',nonce:` + quotedID + `,result:{i:` + quotedID + `,state:` + dv + `,ts:Date.now()}},'*');} catch {}};})();`
}

const (
	capVarLetters = "abcdefghijklmnopqrstuvwxyz"
	capVarChars   = "abcdefghijklmnopqrstuvwxyz0123456789"
)

func capVar(length int) string {
	if length == 0 {
		length = rand.IntN(7) + 4
	}
	name := []byte{capVarLetters[rand.IntN(len(capVarLetters))]}
	for len(name) < length {
		name = append(name, capVarChars[rand.IntN(len(capVarChars))])
	}
	return string(name)
}

func capRand(low, high int) int { return low + rand.IntN(high-low+1) }

func itoa(n int) string   { return strconv.Itoa(n) }
func i32(n int32) string  { return strconv.Itoa(int(n)) }
func u32(n uint32) string { return strconv.FormatUint(uint64(n), 10) }
