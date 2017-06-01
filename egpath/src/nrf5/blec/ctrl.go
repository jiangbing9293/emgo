package blec

import (
	"math/rand"
	"rtos"
	"sync/fence"

	"ble"

	"nrf5/hal/gpio"
	"nrf5/hal/ppi"
	"nrf5/hal/radio"
	"nrf5/hal/rtc"
	"nrf5/hal/timer"
)

type Ctrl struct {
	rnd     rand.XorShift64
	devAddr int64

	advPDU ble.AdvPDU
	rxd    []byte
	txd    []byte

	advIntervalRTC uint32
	connDelay      uint32
	connWindow     uint16

	chi byte
	chm chmap

	isr func(c *Ctrl)

	radio *radio.Periph
	rtc0  *rtc.Periph
	tim0  *timer.Periph

	LEDs *[5]gpio.Pin
}

func (c *Ctrl) RxD() []byte {
	return c.rxd
}

const (
	maxPDULen = 39
	maxPayLen = maxPDULen - 2
)

func NewCtrl() *Ctrl {
	c := new(Ctrl)
	c.devAddr = getDevAddr()
	c.advPDU = ble.MakeAdvPDU(make([]byte, 2+6+3))
	c.advPDU.SetType(ble.AdvInd)
	c.advPDU.SetTxAdd(c.devAddr < 0)
	c.advPDU.AppendAddr(c.devAddr)
	c.advPDU.AppendBytes(ble.Flags, ble.GeneralDisc|ble.OnlyLE)
	c.rxd = make([]byte, maxPDULen)
	c.radio = radio.RADIO
	c.rtc0 = rtc.RTC0
	c.tim0 = timer.TIMER0
	c.rnd.Seed(rtos.Nanosec())
	return c
}

func (c *Ctrl) SetTxPwr(dBm int) {
	c.radio.StoreTXPOWER(dBm)
}

func (c *Ctrl) DevAddr() int64 {
	return c.devAddr
}

// InitPhy initialises physical layer: radio, timers, PPI.
func (c *Ctrl) InitPhy() {
	radioInit(c.radio)
	rtcInit(c.rtc0)
	timerInit(c.tim0)
	ppm := ppi.RADIO_BCMATCH__AAR_START.Mask() |
		ppi.RADIO_READY__CCM_KSGEN.Mask() |
		ppi.RADIO_ADDRESS__CCM_CRYPT.Mask() |
		ppi.RADIO_ADDRESS__TIMER0_CAPTURE1.Mask() |
		ppi.RADIO_END__TIMER0_CAPTURE2.Mask() |
		ppi.RTC0_COMPARE0__RADIO_TXEN.Mask() |
		ppi.RTC0_COMPARE0__RADIO_RXEN.Mask() |
		ppi.RTC0_COMPARE0__TIMER0_CLEAR.Mask() |
		ppi.RTC0_COMPARE0__TIMER0_START.Mask() |
		ppi.TIMER0_COMPARE0__RADIO_TXEN.Mask() |
		ppi.TIMER0_COMPARE0__RADIO_RXEN.Mask() |
		ppi.TIMER0_COMPARE1__RADIO_DISABLE.Mask()
	ppm.Disable()
}

func (c *Ctrl) Advertise(respPDU ble.AdvPDU, advIntervalms int) {
	r := c.radio
	r.StoreCRCINIT(0x555555)
	r.Event(radio.DISABLED).EnableIRQ()
	radioSetAA(r, 0x8E89BED6)
	radioSetMaxLen(r, maxPayLen)
	radioSetChi(r, 37)

	c.chi = 37
	c.txd = respPDU.Bytes()
	c.advIntervalRTC = uint32(advIntervalms*32768+500) / 1000
	c.scanDisabledISR()

	// TIMER0_COMPARE1 is used to implement Rx timeout during advertising
	// and periodical events during connection.
	ppi.TIMER0_COMPARE1__RADIO_DISABLE.Enable()
	ppi.RADIO_ADDRESS__TIMER0_CAPTURE1.Enable()
	ppi.RADIO_END__TIMER0_CAPTURE2.Enable()

	// RTC0_COMPARE0 is used to implement periodical Tx events.
	rt := c.rtc0
	rt.Event(rtc.COMPARE(0)).EnablePPI()
	rt.Task(rtc.CLEAR).Trigger()
	rt.StoreCC(0, 1)
	ppi.RTC0_COMPARE0__RADIO_TXEN.Enable()

	fence.W()
	rt.Task(rtc.START).Trigger()
}

func (c *Ctrl) RadioISR() {
	r := c.radio
	if ev := r.Event(radio.DISABLED); ev.IsSet() {
		ev.Clear()
		c.isr(c)
		return
	}

	r.Event(radio.PAYLOAD).DisableIRQ()

	pdu := ble.AsAdvPDU(c.rxd)
	if len(pdu.Payload()) < 12 ||
		decodeDevAddr(pdu.Payload()[6:], pdu.RxAdd()) != c.devAddr {
		return
	}
	switch {
	case pdu.Type() == ble.ScanReq && len(pdu.Payload()) == 6+6:
		// Setup for TxScanRsp.
		if c.chi != 37 {
			c.chi--
		} else {
			c.chi = 39
		}
		radioSetChi(r, int(c.chi))
		r.StoreSHORTS(radio.READY_START | radio.END_DISABLE |
			radio.DISABLED_TXEN)
		c.isr = (*Ctrl).scanReqRxDisabledISR
		c.LEDs[1].Set()
	case pdu.Type() == ble.ConnectReq && len(pdu.Payload()) == 6+6+22:
		// Setup for connection state.
		r.StoreSHORTS(radio.READY_START | radio.END_DISABLE)
		c.isr = (*Ctrl).connectReqRxDisabledISR
		c.LEDs[2].Set()
	}
}

// scanDisabledISR handles DISABLED->(TXEN/NOP) between RxTxScan* and TxAdvInd.
func (c *Ctrl) scanDisabledISR() {
	c.LEDs[4].Clear()

	r := c.radio

	// Must be before TxAdvInd.START.
	radioSetPDU(r, c.advPDU.Bytes())

	// Must be before TxAdvInd.PAYLOAD
	r.Event(radio.PAYLOAD).DisableIRQ()

	// Must be before TxAdvInd.DISABLED.
	r.StoreSHORTS(radio.READY_START | radio.END_DISABLE | radio.DISABLED_RXEN)
	c.isr = (*Ctrl).advIndTxDisabledISR

	c.tim0.Task(timer.STOP).Trigger()

	if c.chi == 39 {
		rt := c.rtc0
		rt.StoreCC(0, rt.LoadCC(0)+c.advIntervalRTC+c.rnd.Uint32()&255)
	}
}

// advIndTxDisabledISR handles DISABLED->RXEN between TxAdvInd and RxScanReq.
func (c *Ctrl) advIndTxDisabledISR() {
	c.LEDs[4].Set()

	r := c.radio

	// Must be before RxScanReq.START.
	radioSetPDU(r, c.rxd)

	// Must be before RxScanReq.PAYLOAD
	ev := r.Event(radio.PAYLOAD)
	ev.Clear()
	ev.EnableIRQ()

	// Enable RxScanReq timeout.
	t := c.tim0
	t.Task(timer.CLEAR).Trigger()
	t.StoreCC(1, 200) // > t_IFS+t_Preamble+t_AA = 190 µs.
	t.Task(timer.START).Trigger()

	c.setupTxAdvInd()
}

// scanReqRxDisabledISR handles DISABLED->TXEN between RxScanReq and TxScanRsp.
func (c *Ctrl) scanReqRxDisabledISR() {
	// Must be before TxScanRsp.START.
	radioSetPDU(c.radio, c.txd)

	// Must be before TxScanRsp.DISABLED.
	c.setupTxAdvInd()

	c.LEDs[1].Clear()
}

func (c *Ctrl) setupTxAdvInd() {
	// Calculate next channel index. Setup shorts and wakeup time.
	shorts := radio.READY_START | radio.END_DISABLE | radio.DISABLED_TXEN
	if c.chi != 39 {
		c.chi++
	} else {
		c.chi = 37
		shorts &^= radio.DISABLED_TXEN
	}

	// Must be before RxTxScan*.DISABLED
	r := c.radio
	r.StoreSHORTS(shorts)
	radioSetChi(r, int(c.chi))

	c.isr = (*Ctrl).scanDisabledISR
}

// setRxTimers setups RTC0 and Timer0 to trigger RXEN event or timeout DISABLE
// event using base, delay, window in µs.
func (c *Ctrl) setRxTimers(base, delay, window uint32) {
	t := c.tim0
	rt := c.rtc0
	t.Task(timer.CAPTURE(0)).Trigger()
	baseRTC := rt.LoadCOUNTER()
	t.Task(timer.STOP).Trigger()
	t.Task(timer.CLEAR).Trigger()

	delay -= t.LoadCC(0) - base
	rtcTick := delay * 67 / 2048         // 67/2048 < 32768/1e6 == 512/15625
	timTick := delay - rtcTick*15625/512 // µs

	rt.StoreCC(0, baseRTC+rtcTick)
	if timTick < 4 {
		t.StoreCC(1, window)
		ppm := ppi.RTC0_COMPARE0__RADIO_TXEN.Mask() |
			ppi.TIMER0_COMPARE0__RADIO_RXEN.Mask()
		ppm.Disable()
		ppm = ppi.RTC0_COMPARE0__RADIO_RXEN.Mask() |
			ppi.RTC0_COMPARE0__TIMER0_START.Mask()
		ppm.Enable()
	} else {
		t.StoreCC(0, timTick)
		t.StoreCC(1, timTick+window)
		ppm := ppi.RTC0_COMPARE0__RADIO_TXEN.Mask() |
			ppi.RTC0_COMPARE0__RADIO_RXEN.Mask()
		ppm.Disable()
		ppm = ppi.RTC0_COMPARE0__TIMER0_START.Mask() |
			ppi.TIMER0_COMPARE0__RADIO_RXEN.Mask()
		ppm.Enable()
	}
}

func (c *Ctrl) connectReqRxDisabledISR() {
	const (
		rxRU    = 130 // RADIO Rx ramp up (µs).
		rtcRA   = 30  // RTC read accuracy (µs): read <= real <= read+rtcRA.
		aaDelay = 40  // Delay between start of packet and ADDRESS event (µs).
	)

	r := c.radio
	if !r.LoadCRCSTATUS() {
		// Return to advertising.
		r.Task(radio.TXEN).Trigger()
		c.scanDisabledISR()
		c.LEDs[2].Clear()
		return
	}
	// Both timers (RTC0, TIMER0) are running. TIMER0.CC2 contains time of
	// END event (end of ConnectReq packet).

	d := llData(c.rxd[2+6+6:])
	c.chm = d.ChM()

	r.Event(radio.ADDRESS).Clear()
	r.StoreCRCINIT(d.CRCInit())
	radioSetAA(r, d.AA())
	radioSetChi(r, c.chm.NextChi())
	r.StoreSHORTS(radio.READY_START | radio.END_DISABLE | radio.DISABLED_TXEN)

	rsca := d.SCA() + (100<<19+999999)/1000000 // Assume 100 ppm local SCA.

	winOffset := d.WinOffset()
	sca := (winOffset*rsca + 1<<19 - 1) >> 19 // Absolute SCA for (µs).

	// Setup first anchor point.
	c.setRxTimers(
		c.tim0.LoadCC(2),
		winOffset-rxRU-sca,
		d.WinSize()+rxRU+2*sca+rtcRA,
	)
	c.isr = (*Ctrl).connRxDisabledISR

	connInterval := d.Interval()
	sca = (connInterval*rsca + 1<<19 - 1) >> 19
	c.connDelay = connInterval - rxRU - sca - aaDelay
	c.connWindow = uint16(rxRU + 2*sca + rtcRA)
}

var txDataPDU [maxPDULen]byte

func (c *Ctrl) connRxDisabledISR() {
	r := c.radio
	if !r.Event(radio.ADDRESS).IsSet() || !r.LoadCRCSTATUS() {
		c.tim0.Task(timer.STOP).Trigger()
		c.LEDs[2].Clear()
		return
	}
	// Both timers (TIM0, TIMER0) are running. TIMER0.CC1 contains time of
	// ADDRESS event (end of address field of data packet).

	// Must be before ConnTx.START.
	rxd := c.rxd
	txd := txDataPDU[:]
	radioSetPDU(r, txd)
	txd[0] = dcL2CAPCont | rxd[0]&dcNESN<<1 | rxd[0]&dcSN>>1
	txd[1] = 0

	// Must be before ConnTx.DISABLED
	r.StoreSHORTS(radio.READY_START | radio.END_DISABLE)
	c.isr = (*Ctrl).connTxDisabledISR

	c.setRxTimers(c.tim0.LoadCC(1), c.connDelay, uint32(c.connWindow))

	c.LEDs[0].Store(int(txd[0]) >> 3)
}

func (c *Ctrl) connTxDisabledISR() {
	r := c.radio
	radioSetChi(r, c.chm.NextChi())
	radioSetPDU(r, c.rxd)
	r.StoreSHORTS(radio.READY_START | radio.END_DISABLE | radio.DISABLED_TXEN)

	c.isr = (*Ctrl).connRxDisabledISR

	c.LEDs[3].Store(int(txDataPDU[0]) >> 2)
}
