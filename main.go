package main

import (
	"flag"
	"fmt"
	"net"
	"sync"
	"text/tabwriter"
	"time"

	"os"

	"github.com/42wim/ipisp"
	"github.com/Sirupsen/logrus"
	"github.com/briandowns/spinner"
	"github.com/dustin/go-humanize"
	"github.com/miekg/dns"
)

var (
	resolver            = "8.8.8.8"
	wc                  chan string
	done                chan struct{}
	flagScan, flagDebug *bool
	flagQPS             *int
	log                 = logrus.New()
)

type NSInfo struct {
	Name string
	IP   []net.IP
}

type IPInfo struct {
	Loc string
	ASN ipisp.ASN
	ISP string
}

type KeyInfo struct {
	Start int64
	End   int64
}

func ipinfo(ip net.IP) (IPInfo, error) {
	client, _ := ipisp.NewDNSClient()
	resp, err := client.LookupIP(net.ParseIP(ip.String()))
	if err != nil {
		return IPInfo{}, err
	}
	return IPInfo{resp.Country, resp.ASN, resp.Name.Raw}, nil
}

func getIP(host string, qtype uint16, server string) []net.IP {
	var ips []net.IP
	rrset, _, err := queryRRset(host, qtype, server, false)
	if err != nil {
		return ips
	}
	for _, rr := range rrset {
		switch rr.(type) {
		case *dns.A:
			ips = append(ips, rr.(*dns.A).A)
		case *dns.AAAA:
			ips = append(ips, rr.(*dns.AAAA).AAAA)
		}
	}
	return ips
}

func extractRR(rrset []dns.RR, qtype uint16) []dns.RR {
	var out []dns.RR
	for _, rr := range rrset {
		if rr.Header().Rrtype == qtype {
			out = append(out, rr)
		}
	}
	return out
}

func query(q string, qtype uint16, server string, sec bool) (*dns.Msg, time.Duration, error) {
	c := new(dns.Client)
	m := prepMsg()
	m.CheckingDisabled = true
	if sec {
		m.CheckingDisabled = false
		m.SetEdns0(4096, true)
	}
	m.Question[0] = dns.Question{dns.Fqdn(q), qtype, dns.ClassINET}
	in, rtt, err := c.Exchange(m, net.JoinHostPort(server, "53"))
	if err != nil {
		return nil, 0, err
	}
	return in, rtt, nil
}

func queryRRset(q string, qtype uint16, server string, sec bool) ([]dns.RR, time.Duration, error) {
	res, rtt, err := query(q, qtype, server, sec)
	if err != nil {
		return []dns.RR{}, 0, err
	}
	rrset := extractRR(res.Answer, qtype)
	if len(rrset) == 0 {
		return []dns.RR{}, 0, fmt.Errorf("no rr for %#v", qtype)
	}
	return rrset, rtt, nil
}

func findNS(domain string) ([]NSInfo, error) {
	rrset, _, err := queryRRset(domain, dns.TypeNS, resolver, false)
	if err != nil {
		return []NSInfo{}, nil
	}
	var nsinfos []NSInfo
	for _, rr := range rrset {
		ns := rr.(*dns.NS).Ns
		nsinfo := NSInfo{}
		ips := []net.IP{}
		nsinfo.Name = ns
		ips = append(ips, getIP(ns, dns.TypeA, resolver)...)
		ips = append(ips, getIP(ns, dns.TypeAAAA, resolver)...)
		nsinfo.IP = ips
		nsinfos = append(nsinfos, nsinfo)
	}
	if len(nsinfos) == 0 {
		return nsinfos, fmt.Errorf("no NS found")
	}
	return nsinfos, nil
}

func prepMsg() *dns.Msg {
	m := new(dns.Msg)
	m.Id = dns.Id()
	m.RecursionDesired = true
	m.Question = make([]dns.Question, 1)
	return m
}

func outputter() {
	const padding = 1
	w := tabwriter.NewWriter(os.Stdout, 0, 0, padding, ' ', tabwriter.Debug)
	for input := range wc {
		fmt.Fprintf(w, input)
	}
	w.Flush()
	done <- struct{}{}
}

func main() {
	flagDebug = flag.Bool("debug", false, "enable debug")
	flagScan = flag.Bool("scan", false, "scan domain for common records")
	flagQPS = flag.Int("qps", 10, "Queries per seconds (per nameserver)")
	flag.Parse()

	if len(flag.Args()) == 0 {
		fmt.Println("Usage:")
		fmt.Println("\tdt [FLAGS] domain")
		fmt.Println()
		fmt.Println("Example:")
		fmt.Println("\tdt icann.org")
		fmt.Println("\tdt -debug ripe.net")
		fmt.Println("\tdt -debug -scan yourdomain.com")
		fmt.Println()
		fmt.Println("Flags:")
		flag.PrintDefaults()
		return
	}

	if *flagDebug {
		log.Level = logrus.DebugLevel
	}

	domain := flag.Arg(0)
	nsinfos, err := findNS(dns.Fqdn(domain))
	if len(nsinfos) == 0 {
		fmt.Println("no nameservers found for", domain)
		return
	}
	if err != nil {
		fmt.Println(err)
		return
	}

	s := spinner.New(spinner.CharSets[14], 100*time.Millisecond)
	s.Start()

	// check dnssec
	chainValid, chainErr := validateChain(dns.Fqdn(domain))

	wc = make(chan string)
	done = make(chan struct{})
	var wg sync.WaitGroup
	go outputter()

	wc <- fmt.Sprintf("NS\tIP\tLOC\tASN\tISP\trtt\tSerial\tDNSSEC\tValidFrom\tValidUntil\n")

	// for now disable debuglevel (because of multiple goroutines output)
	if *flagDebug {
		log.Level = logrus.InfoLevel
	}
	for _, nsinfo := range nsinfos {
		wg.Add(1)
		go func(nsinfo NSInfo) {
			output := fmt.Sprintf("%s\t", nsinfo.Name)
			i := 0
			for _, ip := range nsinfo.IP {
				info, _ := ipinfo(ip)
				if i > 0 {
					output = output + fmt.Sprintf("\t%s\t", ip.String())
				} else {
					output = output + fmt.Sprintf("%s\t", ip.String())
				}
				output = output + fmt.Sprintf("%v\tASN %#v\t%v\t", info.Loc, info.ASN, fmt.Sprintf("%.40s", info.ISP))
				soa, rtt, err := queryRRset(domain, dns.TypeSOA, ip.String(), false)
				if err != nil {
					output = output + fmt.Sprintf("%s\t%v\t", "error", "error")
				} else {
					output = output + fmt.Sprintf("%s\t%v\t", rtt.String(), int64(soa[0].(*dns.SOA).Serial))
				}
				keys, _, err := queryRRset(domain, dns.TypeDNSKEY, ip.String(), true)
				if err != nil {
				}
				res, _, err := query(domain, dns.TypeNS, ip.String(), true)
				if err != nil {
				}
				valid, keyinfo, err := validateRRSIG(keys, res.Answer)
				if valid && chainValid {
					output = output + fmt.Sprintf("%v\t%s\t%s", "valid", humanize.Time(time.Unix(keyinfo.Start, 0)), humanize.Time(time.Unix(keyinfo.End, 0)))
				} else {
					if err != nil {
						output = output + fmt.Sprintf("%v\t%s\t%s", "error", "", "")
					} else {
						if keyinfo.Start == 0 && len(keys) == 0 {
							output = output + fmt.Sprintf("%v\t%s\t%s", "disabled", "", "")
						} else {
							output = output + fmt.Sprintf("%v\t%s\t%s", "invalid", humanize.Time(time.Unix(keyinfo.Start, 0)), humanize.Time(time.Unix(keyinfo.End, 0)))
						}
					}
				}
				output = output + fmt.Sprintln()
				i++
			}
			wc <- output
			wg.Done()
		}(nsinfo)
	}
	wg.Wait()
	close(wc)
	s.Stop()
	<-done

	if chainErr != nil {
		fmt.Printf("DNSSEC: %s\n", chainErr)
	}

	// enable debug again if needed
	if *flagDebug {
		log.Level = logrus.DebugLevel
	}

	if *flagScan {
		domainscan(domain)
	}
}

func getParentDomain(domain string) string {
	i, end := dns.NextLabel(domain, 0)
	if !end {
		return domain[i:]
	}
	return "."
}
