package processor

import "log"

// Control signal types
const (
	RebuildRangeFilter = "rebuild_range_filter"
	ReloadBirdNET      = "reload_birdnet"
)

// controlSignalMonitor handles various control signals for the processor
func (p *Processor) controlSignalMonitor() {
	go func() {
		for signal := range p.controlChan {
			switch signal {
			case RebuildRangeFilter:
				if err := p.BuildRangeFilter(); err != nil {
					log.Printf("\033[31m❌ Error handling range filter rebuild: %v\033[0m", err)
				} else {
					log.Printf("\033[32m🔄 Range filter rebuilt successfully\033[0m")
				}
			default:
				log.Printf("Received unknown control signal: %v", signal)
			}
		}
	}()
}
