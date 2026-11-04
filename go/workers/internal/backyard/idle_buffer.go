package backyard

func (p SelectorPolicy) idleBufferRaw(nav int64) int64 {
	if nav <= 0 {
		return 0
	}
	buffer := p.IdleBufferRaw
	if p.IdleBufferBPS > 0 {
		fraction := nav/10_000*p.IdleBufferBPS + nav%10_000*p.IdleBufferBPS/10_000
		buffer = max(buffer, min(fraction, p.IdleBufferCapRaw))
	}
	return min(nav, buffer)
}
