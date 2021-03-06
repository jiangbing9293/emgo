// +build noos
// +build cortexm0 cortexm3 cortexm4 cortexm4f cortexm7f cortexm7d

package syscall

import (
	"arch/cortexm"
)

const (
	IRQPrioLowest  = cortexm.PrioLowest
	IRQPrioHighest = cortexm.PrioHighest
	IRQPrioStep    = cortexm.PrioStep
	IRQPrioNum     = cortexm.PrioNum

	SyscallPrio = cortexm.PrioLowest + cortexm.PrioStep*cortexm.PrioNum/2
)
