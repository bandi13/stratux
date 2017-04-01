package main

import (
	"fmt"
	"log"
	"math"
	"path/filepath"
	"time"

	"../sensors"

	"github.com/kidoman/embd"
	_ "github.com/kidoman/embd/host/all"
	"github.com/westphae/goflying/ahrs"
	"github.com/westphae/goflying/ahrsweb"
)

const numRetries uint8 = 5

var (
	i2cbus           embd.I2CBus
	myPressureReader sensors.PressureReader
	myIMUReader      sensors.IMUReader
	cage             chan (bool)
	analysisLogger   *ahrs.AHRSLogger
	ahrsCalibrating  bool
)

func initI2CSensors() {
	i2cbus = embd.NewI2CBus(1)

	go pollSensors()
	go sensorAttitudeSender()
	go updateAHRSStatus()
}

func pollSensors() {
	timer := time.NewTicker(4 * time.Second)
	for {
		<-timer.C

		// If it's not currently connected, try connecting to pressure sensor
		if globalSettings.BMP_Sensor_Enabled && !globalStatus.BMPConnected {
			log.Println("AHRS Info: attempting pressure sensor connection.")
			globalStatus.BMPConnected = initPressureSensor() // I2C temperature and pressure altitude.
			go tempAndPressureSender()
		}

		// If it's not currently connected, try connecting to IMU
		if globalSettings.IMU_Sensor_Enabled && !globalStatus.IMUConnected {
			log.Println("AHRS Info: attempting IMU connection.")
			globalStatus.IMUConnected = initIMU() // I2C accel/gyro/mag.
		}
	}
}

func initPressureSensor() (ok bool) {
	bmp, err := sensors.NewBMP280(&i2cbus, 100*time.Millisecond)
	if err == nil {
		myPressureReader = bmp
		log.Println("AHRS Info: Successfully initialized BMP280")
		return true
	}

	// TODO westphae: make bmp180.go to fit bmp interface
	//for i := 0; i < 5; i++ {
	//	myBMPX80 = bmp180.New(i2cbus)
	//	_, err := myBMPX80.Temperature() // Test to see if it works, since bmp180.New doesn't return err
	//	if err != nil {
	//		time.Sleep(250 * time.Millisecond)
	//	} else {
	//		globalStatus.BMPConnected = true
	//		log.Println("AHRS Info: Successfully initialized BMP180")
	//		return nil
	//	}
	//}

	log.Println("AHRS Info: couldn't initialize BMP280 or BMP180")
	return false
}

func tempAndPressureSender() {
	var (
		temp     float64
		press    float64
		altLast  float64 = -9999
		altitude float64
		err      error
		dt       float64 = 0.1
		failnum  uint8
	)

	// Initialize variables for rate of climb calc
	u := 5 / (5 + float64(dt)) // Use 5 sec decay time for rate of climb, slightly faster than typical VSI

	timer := time.NewTicker(time.Duration(1000*dt) * time.Millisecond)
	for globalSettings.BMP_Sensor_Enabled && globalStatus.BMPConnected {
		<-timer.C

		// Read temperature and pressure altitude.
		temp, err = myPressureReader.Temperature()
		if err != nil {
			log.Printf("AHRS Error: Couldn't read temperature from sensor: %s", err)
		}
		press, err = myPressureReader.Pressure()
		if err != nil {
			log.Printf("AHRS Error: Couldn't read pressure from sensor: %s", err)
			failnum++
			if failnum > numRetries {
				log.Printf("AHRS Error: Couldn't read pressure from sensor %d times, closing BMP: %s", failnum, err)
				myPressureReader.Close()
				globalStatus.BMPConnected = false // Try reconnecting a little later
				break
			}
		}

		// Update the Situation data.
		mySituation.muBaro.Lock()
		mySituation.BaroLastMeasurementTime = stratuxClock.Time
		mySituation.BaroTemperature = temp
		altitude = CalcAltitude(press)
		mySituation.BaroPressureAltitude = altitude
		if altLast < -2000 {
			altLast = altitude // Initialize
		}
		// Assuming timer is reasonably accurate, use a regular ewma
		mySituation.BaroVerticalSpeed = u*mySituation.BaroVerticalSpeed + (1-u)*(altitude-altLast)/(float64(dt)/60)
		mySituation.muBaro.Unlock()
		altLast = altitude
	}
}

func initIMU() (ok bool) {
	log.Println("AHRS Info: attempting to connect to MPU9250")
	imu, err := sensors.NewMPU9250()
	if err == nil {
		myIMUReader = imu
		time.Sleep(200 * time.Millisecond)
		log.Println("AHRS Info: Successfully connected MPU9250, running calibration")
		ahrsCalibrating = true
		if err := myIMUReader.Calibrate(1, 1); err == nil {
			log.Println("AHRS Info: Successfully calibrated MPU9250")
			ahrsCalibrating = false
			return true
		}
		log.Println("AHRS Info: couldn't calibrate MPU9250")
		ahrsCalibrating = false
		return false
	}

	// TODO westphae: try to connect to MPU9150

	log.Println("AHRS Error: couldn't initialize MPU9250 or MPU9150")
	return false
}

func sensorAttitudeSender() {
	var (
		roll, pitch, heading               float64
		t                                  time.Time
		s                                  ahrs.AHRSProvider
		m                                  *ahrs.Measurement
		a1, a2, a3, b1, b2, b3, m1, m2, m3 float64        // IMU measurements
		ff                                 *[3][3]float64 // Sensor orientation matrix
		mpuError, magError                 error
		failnum                            uint8
	)
	log.Println("AHRS Info: initializing new simple AHRS")
	s = ahrs.InitializeSimple()
	m = ahrs.NewMeasurement()
	cage = make(chan (bool))

	// Set up loggers for analysis
	ahrswebListener, err := ahrsweb.NewKalmanListener()
	if err != nil {
		log.Printf("AHRS Error: couldn't start ahrswebListener: %s\n", err.Error())
	} else {
		defer ahrswebListener.Close()
	}

	// Need a sampling freq faster than 10Hz
	timer := time.NewTicker(50 * time.Millisecond) // ~20Hz update.
	for {
		ff = makeSensorRotationMatrix(m)

		failnum = 0
		<-timer.C
		for globalSettings.IMU_Sensor_Enabled && globalStatus.IMUConnected {
			<-timer.C
			select {
			case <-cage:
				ahrsCalibrating = true
				if err := myIMUReader.Calibrate(1, 1); err == nil {
					log.Println("AHRS Info: Successfully recalibrated MPU9250")
					ff = makeSensorRotationMatrix(m)

				} else {
					log.Println("AHRS Info: couldn't recalibrate MPU9250")
				}
				ahrsCalibrating = false
				s.Reset()
			default:
			}

			t = stratuxClock.Time
			m.T = float64(t.UnixNano()/1000) / 1e6

			_, b1, b2, b3, a1, a2, a3, m1, m2, m3, mpuError, magError = myIMUReader.Read()
			// This is how the RY83XAI is wired up
			//m.A1, m.A2, m.A3 = -a2, +a1, -a3
			//m.B1, m.B2, m.B3 = +b2, -b1, +b3
			//m.M1, m.M2, m.M3 = +m1, +m2, +m3
			// This is how the OpenFlightBox board is wired up
			//m.A1, m.A2, m.A3 = +a1, -a2, +a3
			//m.B1, m.B2, m.B3 = -b1, +b2, -b3
			//m.M1, m.M2, m.M3 = +m2, +m1, +m3
			m.A1 = -(ff[0][0]*a1 + ff[0][1]*a2 + ff[0][2]*a3)
			m.A2 = -(ff[1][0]*a1 + ff[1][1]*a2 + ff[1][2]*a3)
			m.A3 = -(ff[2][0]*a1 + ff[2][1]*a2 + ff[2][2]*a3)
			m.B1 = ff[0][0]*b1 + ff[0][1]*b2 + ff[0][2]*b3
			m.B2 = ff[1][0]*b1 + ff[1][1]*b2 + ff[1][2]*b3
			m.B3 = ff[2][0]*b1 + ff[2][1]*b2 + ff[2][2]*b3
			m.M1 = ff[0][0]*m1 + ff[0][1]*m2 + ff[0][2]*m3
			m.M2 = ff[1][0]*m1 + ff[1][1]*m2 + ff[1][2]*m3
			m.M3 = ff[2][0]*m1 + ff[2][1]*m2 + ff[2][2]*m3
			m.SValid = mpuError == nil
			m.MValid = magError == nil
			if mpuError != nil {
				log.Printf("AHRS Gyro/Accel Error: %s\n", mpuError)
				failnum++
				if failnum > numRetries {
					log.Printf("AHRS Gyro/Accel Error: failed to read %d times, restarting: %s\n",
						failnum-1, mpuError)
					myIMUReader.Close()
					globalStatus.IMUConnected = false
				}
				continue
			}
			failnum = 0
			if magError != nil {
				log.Printf("AHRS Magnetometer Error, not using for this run: %s\n", magError)
				m.MValid = false
				// Don't necessarily disconnect here, unless AHRSProvider deeply depends on magnetometer
			}

			m.TW = float64(mySituation.GPSLastGroundTrackTime.UnixNano()/1000) / 1e6
			m.WValid = t.Sub(mySituation.GPSLastGroundTrackTime) < 3000*time.Millisecond
			if m.WValid {
				m.W1 = mySituation.GPSGroundSpeed * math.Sin(float64(mySituation.GPSTrueCourse)*ahrs.Deg)
				m.W2 = mySituation.GPSGroundSpeed * math.Cos(float64(mySituation.GPSTrueCourse)*ahrs.Deg)
				if globalSettings.BMP_Sensor_Enabled && globalStatus.BMPConnected {
					m.W3 = mySituation.BaroVerticalSpeed * 60 / 6076.12
				} else {
					m.W3 = float64(mySituation.GPSVerticalSpeed) * 3600 / 6076.12
				}
			}

			// Run the AHRS calcs
			s.Compute(m)

			makeAHRSGDL90Report() // Send whether or not valid - the function will invalidate the values as appropriate

			// If we have valid AHRS info, then update mySituation
			if s.Valid() {
				mySituation.muAttitude.Lock()

				roll, pitch, heading = s.RollPitchHeading()
				mySituation.AHRSRoll = roll / ahrs.Deg
				mySituation.AHRSPitch = pitch / ahrs.Deg
				mySituation.AHRSGyroHeading = heading / ahrs.Deg

				mySituation.AHRSMagHeading = s.MagHeading()
				mySituation.AHRSSlipSkid = s.SlipSkid()
				mySituation.AHRSTurnRate = s.RateOfTurn()
				mySituation.AHRSGLoad = s.GLoad()

				mySituation.AHRSLastAttitudeTime = t
				mySituation.muAttitude.Unlock()

				// makeFFAHRSSimReport() // simultaneous use of GDL90 and FFSIM not supported in FF 7.5.1 or later. Function definition will be kept for AHRS debugging and future workarounds.
			} else {
				s.Reset()
			}

			// Debugging server:
			if ahrswebListener != nil {
				if err = ahrswebListener.Send(s.GetState(), m); err != nil {
					log.Printf("Error writing to ahrsweb: %s\n", err)
					ahrswebListener = nil
				}
			}

			// Log it to csv for analysis
			if globalSettings.AHRSLog && usage.Usage() < 0.95 {
				if analysisLogger == nil {
					analysisFilename := filepath.Join(logDirf, fmt.Sprintf("sensors_%s.csv",
						time.Now().Format("20060102_150405")))
					analysisLogger = ahrs.NewAHRSLogger(analysisFilename, s.GetLogMap())
				}

				if analysisLogger != nil {
					analysisLogger.Log()
				}
			} else {
				analysisLogger = nil
			}
		}
	}
}

func makeSensorRotationMatrix(mCal *ahrs.Measurement) (ff *[3][3]float64) {
	if globalSettings.IMUMapping[0] == 0 { // if unset, default to RY836AI in standard orientation
		globalSettings.IMUMapping[0] = -1 // +2
		globalSettings.IMUMapping[1] = -3 // +3
		saveSettings()
	}
	f := globalSettings.IMUMapping
	ff = new([3][3]float64)
	// TODO westphae: remove the projection on the measured gravity vector so it's orthogonal.
	if f[0] < 0 { // This is the "forward direction" chosen for the sensor.
		ff[0][-f[0]-1] = -1
	} else {
		ff[0][+f[0]-1] = +1
	}
	//TODO westphae: replace "up direction" with opposite of measured gravity.
	if f[1] < 0 { // This is the "up direction" chosen for the sensor.
		ff[2][-f[1]-1] = -1
	} else {
		ff[2][+f[1]-1] = +1
	}
	// This specifies the "left wing" direction for a right-handed coordinate system.
	ff[1][0] = ff[2][1]*ff[0][2] - ff[2][2]*ff[0][1]
	ff[1][1] = ff[2][2]*ff[0][0] - ff[2][0]*ff[0][2]
	ff[1][2] = ff[2][0]*ff[0][1] - ff[2][1]*ff[0][0]
	return ff
}

// This is used in the orientation process where the user specifies the forward and up directions.
func getMinAccelDirection() (i int, err error) {
	_, _, _, _, a1, a2, a3, _, _, _, err, _ := myIMUReader.Read()
	if err != nil {
		return
	}
	log.Printf("AHRS Info: sensor orientation accels %1.3f %1.3f %1.3f\n", a1, a2, a3)
	switch {
	case math.Abs(a1) > math.Abs(a2) && math.Abs(a1) > math.Abs(a3):
		if a1 > 0 {
			i = 1
		} else {
			i = -1
		}
	case math.Abs(a2) > math.Abs(a3) && math.Abs(a2) > math.Abs(a1):
		if a2 > 0 {
			i = 2
		} else {
			i = -2
		}
	case math.Abs(a3) > math.Abs(a1) && math.Abs(a3) > math.Abs(a2):
		if a3 > 0 {
			i = 3
		} else {
			i = -3
		}
	default:
		err = fmt.Errorf("couldn't determine biggest accel from %1.3f %1.3f %1.3f", a1, a2, a3)
	}

	return
}

// CageAHRS sends a signal to the AHRSProvider that it should be reset.
func CageAHRS() {
	cage <- true
}

func updateAHRSStatus() {
	var (
		msg    uint8
		imu    bool
		ticker *time.Ticker
	)

	ticker = time.NewTicker(250 * time.Millisecond)

	for {
		<-ticker.C
		msg = 0

		// GPS valid
		if stratuxClock.Time.Sub(mySituation.GPSLastGroundTrackTime) < 3000*time.Millisecond {
			msg++
		}
		// IMU is being used
		imu = globalSettings.IMU_Sensor_Enabled && globalStatus.IMUConnected
		if imu {
			msg += 1 << 1
		}
		// BMP is being used
		if globalSettings.BMP_Sensor_Enabled && globalStatus.BMPConnected {
			msg += 1 << 2
		}
		// IMU is doing a calibration
		if ahrsCalibrating {
			msg += 1 << 3
		}
		// Logging to csv
		if imu && analysisLogger != nil {
			msg += 1 << 4
		}
		mySituation.AHRSStatus = msg
	}
}