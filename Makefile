
ifeq "$(CIRCLECI)" "true"
	BUILDINFO=
	PLATFORMDEPENDENT=
else
	LDFLAGS_VERSION=-X main.stratuxVersion=`git describe --tags --abbrev=0` -X main.stratuxBuild=`git log -n 1 --pretty=%H`
	BUILDINFO=-ldflags "$(LDFLAGS_VERSION)"
	BUILDINFO_STATIC=-ldflags "-extldflags -static $(LDFLAGS_VERSION)"
	PLATFORMDEPENDENT=fancontrol
endif

all: xdump978 xdump1090 xgen_gdl90 $(PLATFORMDEPENDENT)

xgen_gdl90:
	go get -t -d -v ./main ./test ./godump978 ./uatparse
	go build -v $(BUILDINFO) -i main/gen_gdl90.go main/traffic.go main/gps.go main/network.go main/managementinterface.go main/sdr.go main/ping.go main/uibroadcast.go main/monotonic.go main/datalog.go main/equations.go main/cputemp.go

fancontrol:
	go get -t -d -v ./main
	go build $(BUILDINFO) -i main/fancontrol.go main/equations.go main/cputemp.go

xdump1090:
	git submodule update --init
	cd dump1090 && $(MAKE)

xdump978:
	cd dump978 && $(MAKE) lib

.PHONY: test
test:
	$(MAKE) -C test	

www:
	cd web && $(MAKE)

install:
	cp -f gen_gdl90 /usr/bin/gen_gdl90
	chmod 755 /usr/bin/gen_gdl90
	cp -f fancontrol /usr/bin/fancontrol
	chmod 755 /usr/bin/fancontrol
	-/usr/bin/fancontrol remove
	/usr/bin/fancontrol install
	cp image/10-stratux.rules /etc/udev/rules.d/10-stratux.rules
	cp image/99-uavionix.rules /etc/udev/rules.d/99-uavionix.rules
	rm -f /etc/init.d/stratux
	cp __lib__systemd__system__stratux.service /lib/systemd/system/stratux.service
	cp __root__stratux-pre-start.sh /root/stratux-pre-start.sh
	chmod 644 /lib/systemd/system/stratux.service
	chmod 744 /root/stratux-pre-start.sh
	ln -fs /lib/systemd/system/stratux.service /etc/systemd/system/multi-user.target.wants/stratux.service
	$(MAKE) www
	cp -f libdump978.so /usr/lib/libdump978.so
	cp -f dump1090/dump1090 /usr/bin/
	cp -f image/hostapd_manager.sh /usr/sbin/
	cp -f image/stratux-wifi.sh /usr/sbin/

clean:
	rm -f gen_gdl90 libdump978.so fancontrol
	cd dump1090 && $(MAKE) clean
	cd dump978 && $(MAKE) clean
