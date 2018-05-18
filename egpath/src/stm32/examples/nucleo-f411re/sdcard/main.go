package main

import (
	"delay"
	"fmt"

	"sdcard"

	"stm32/hal/dma"
	"stm32/hal/gpio"
	"stm32/hal/system"
	"stm32/hal/system/timer/systick"

	"stm32/hal/raw/rcc"
)

var h Host

func init() {
	system.Setup96(8) // Setups USB/SDIO/RNG clock to 48 MHz
	systick.Setup(2e6)

	// GPIO

	gpio.A.EnableClock(true)
	cmd := gpio.A.Pin(6)
	//d1 := gpio.A.Pin(8)
	//d2 := gpio.A.Pin(9)

	gpio.B.EnableClock(true)
	//d3 := gpio.B.Pin(5)
	d0 := gpio.B.Pin(7)
	clk := gpio.B.Pin(15)

	cfg := gpio.Config{Mode: gpio.Alt, Speed: gpio.High, Pull: gpio.PullUp}
	for _, pin := range []gpio.Pin{clk, cmd, d0 /*d1, d2, d3*/} {
		pin.Setup(&cfg)
		pin.SetAltFunc(gpio.SDIO)
	}

	d := dma.DMA2
	d.EnableClock(true)
	h.dma = d.Channel(6, 4)

	rcc.RCC.SDIOEN().Set()
	h.Enable()
	h.SetFreq(400e3) // Set SDIO_CK to no more than 400 kHz (max. OD freq).
}

func main() {
	delay.Millisec(250) // For SWO output

	ocr := sdcard.V31 | sdcard.V32 | sdcard.V33 | sdcard.HCXC
	v2 := true

	fmt.Printf("\nInitializing SD card ")

	// Reset
	h.Cmd(sdcard.CMD0())
	checkErr("CMD0", h.Err(true), 0)

	// Verify card interface operating condition.
	vhs, pattern := h.Cmd(sdcard.CMD8(sdcard.V27_36, 0xAC)).R7()
	if err := h.Err(true); err != nil {
		if err == sdcard.ErrTimeout {
			ocr &^= sdcard.HCXC
			v2 = false
		} else {
			checkErr("CMD8", err, 0)
		}
	} else if vhs != sdcard.V27_36 || pattern != 0xAC {
		fmt.Printf("CMD8 bad response: %x, %x\n", vhs, pattern)
		for {
		}
	}

	// Initialize memory card.
	var oca sdcard.OCR
	for i := 0; oca&sdcard.PWUP == 0 && i < 20; i++ {
		h.Cmd(sdcard.CMD55(0))
		oca = h.Cmd(sdcard.ACMD41(ocr)).R3()
		checkErr("ACMD41", h.Err(true), 0)
		fmt.Printf(".")
		delay.Millisec(50)
	}
	if oca&sdcard.PWUP == 0 {
		fmt.Printf(" timeout\n")
		for {
		}
	}
	fmt.Printf(" OK\n\n")
	fmt.Printf("Physicaly layer version 2.00+: %t\n", v2)
	fmt.Printf("Operation Conditions Register: 0x%08X\n\n", oca)

	// Read Card Identification Register.
	cid := h.Cmd(sdcard.CMD2()).R2CID()
	checkErr("CMD2", h.Err(true), 0)

	printCID(cid)

	// Generate new Relative Card Address.
	rca, _ := h.Cmd(sdcard.CMD3()).R6()
	checkErr("CMD3", h.Err(true), 0)

	fmt.Printf("Relative Card Address: 0x%04X\n\n", rca)

	// After CMD3 card is in Data Transfer Mode (Standby State) and SDIO_CK can
	// be set to no more than 25 MHz (max. PP freq).
	h.SetFreq(25e6)

	// Read Card Specific Data register.
	csd := h.Cmd(sdcard.CMD9(rca)).R2CSD()
	checkErr("CMD9", h.Err(true), 0)

	printCSD(csd)

	// Select card (put into Transfer State).
	cs := h.Cmd(sdcard.CMD7(rca)).R1()
	checkErr("CMD7", h.Err(true), cs)

	// Disable 50k pull-up resistor on D3/CD.
	h.Cmd(sdcard.CMD55(rca))
	cs = h.Cmd(sdcard.ACMD42(false)).R1()
	checkErr("ACMD42", h.Err(true), cs)

}