package scan

import (
	"fmt"
	"time"

	"github.com/42wim/dt/structs"
	"github.com/miekg/dns"
)

func validateDNSKEY(keys []dns.RR) (bool, structs.KeyInfo, error) {
	return validateRRSIG(keys, keys)
}

func (s *Scan) ValidateRRSIG(keys []dns.RR, rrset []dns.RR) (bool, structs.KeyInfo, error) {
	return validateRRSIG(keys, rrset)
}

func validateRRSIG(keys []dns.RR, rrset []dns.RR) (bool, structs.KeyInfo, error) {
	if len(rrset) == 0 {
		return false, structs.KeyInfo{}, nil
	}
	var sig *dns.RRSIG
	var cleanset []dns.RR
	for _, v := range rrset {
		_, ok := v.(*dns.RRSIG)
		if ok {
			sig = v.(*dns.RRSIG)
		} else {
			cleanset = append(cleanset, v)
		}
	}
	for _, k := range keys {
		if _, ok := k.(*dns.DNSKEY); !ok {
			//fmt.Println("not ok, skipping")
			continue
		}
		key := k.(*dns.DNSKEY)
		log.Debugf("Trying validation RRSIG with DNSKEY %s (flag %v, keytag %v)", key.PublicKey, key.Flags, key.KeyTag())
		err := sig.Verify(key, cleanset)
		if err == nil {
			ti, te := explicitValid(sig)
			if sig.ValidityPeriod(time.Now()) {
				log.Debugf("Validation succeeded")
				return true, structs.KeyInfo{
					Start: ti,
					End:   te,
				}, nil
			}
			//	return false, structs.KeyInfo{ti, te}, nil
		}
		log.Debugf("Validation failed")
	}
	return false, structs.KeyInfo{}, nil
}

func explicitValid(rr *dns.RRSIG) (int64, int64) {
	t := time.Now()
	var utc int64
	var year68 = int64(1 << 31)
	if t.IsZero() {
		utc = time.Now().UTC().Unix()
	} else {
		utc = t.UTC().Unix()
	}
	modi := (int64(rr.Inception) - utc) / year68
	mode := (int64(rr.Expiration) - utc) / year68
	ti := int64(rr.Inception) + (modi * year68)
	te := int64(rr.Expiration) + (mode * year68)
	return ti, te
}

func (s *Scan) ValidateChain(domain string) (bool, error) {
	return s.validateChain(domain)
}

func (s *Scan) validateChain(domain string) (bool, error) {
	for {
		log.Debugf("Validating %s", domain)
		valid, err := s.validateDomain(domain)
		if err != nil {
			return false, err
		}
		if !valid {
			return false, fmt.Errorf("validateChain failed. Run with -debug for more information")
		}
		parent := getParentDomain(domain)
		if parent == "." {
			return true, nil
		}
		domain = parent
	}
}

func (s *Scan) LookupDNSKEY(domain string, nsip string, keyMap map[uint16]*dns.DNSKEY) (structs.Response, error) {
	found := false
	res, err := query(domain, dns.TypeDNSKEY, nsip, true)
	if err != nil {
		log.Debugf("error %s", err)
		return res, nil
	}
	// map DNSKEYs
	for _, a := range res.Msg.Answer {
		switch key := a.(type) {
		case *dns.DNSKEY:
			found = true
			if exist, ok := keyMap[key.KeyTag()]; ok {
				if key.PublicKey != exist.PublicKey {
					return res, fmt.Errorf("validation failed. DNSKEY with same keytag differ")
				}
			}
			keyMap[key.KeyTag()] = key
		}
	}
	if !found {
		return res, fmt.Errorf("validation failed. No DNSKEY found for %s on %s", domain, nsip)
	}
	return res, nil
}

func (s *Scan) validateParentDS(domain string, keyMap map[uint16]*dns.DNSKEY) (bool, error) {
	// get auth servers of parent
	log.Debugf("Finding NS of parent: %s", dns.Fqdn(getParentDomain(domain)))
	nsdata, err := s.FindNS(getParentDomain(domain))
	if err != nil {
		log.Debugf("ValidateDomain() error: %#v", err)
	}

	// asking parent about DS
	foundKeyTag := false
	for _, ns := range nsdata {
		for _, nsip := range ns.IP {
			log.Debugf("Asking parent %s (%s) DS of %s", ns.Name, nsip.String(), domain)
			res, err := query(domain, dns.TypeDS, nsip.String(), true)
			if err == nil && len(res.Msg.Answer) == 0 {
				return false, fmt.Errorf("validation failed. No DS records found for %s on %v", domain, nsip.String())
			}
			if err != nil {
				log.Debugf("error %s", err)
				break
			}
			// look for all parent DS and compare digests
			for _, a := range res.Msg.Answer {
				switch parentDS := a.(type) {
				case *dns.DS:
					// does the child has a DNSKEY with the found KeyTag ?
					key := keyMap[parentDS.KeyTag]
					if key == nil {
						log.Debugf("No DNSKEY (keytag %v) in %s found that matches DS (keytag %v) in %s", parentDS.KeyTag, domain, parentDS.KeyTag, nsip.String())
						continue
					}
					if parentDS.DigestType == 3 {
						// no support for GOST for now
						break
					}
					foundKeyTag = true
					// create the child digest based on the parentDS digesttype
					childDS := key.ToDS(parentDS.DigestType)
					// if this doesn't fail (shouldn't be happening?)
					if childDS != nil {
						log.Debugf("parent DS digest: %s (keytag %v, type %v)", parentDS.Digest, parentDS.KeyTag, parentDS.DigestType)
						log.Debugf("child DS digest %s (keytag %v, type %v)", childDS.Digest, childDS.KeyTag, childDS.DigestType)
						if parentDS.Digest == childDS.Digest {
							log.Debugf("%s validated", domain)
							//return true, nil
						} else {
							log.Debugf("%s failure", domain)
							return false, nil
						}
					} else {
						log.Debugf("childDS is nil ? shouldn't be happening %v %v %v", parentDS.KeyTag, key.PublicKey, parentDS.DigestType)
					}
				}
			}
		}
	}
	if !foundKeyTag {
		log.Debugf("Validation failed. No DNSKEY in %s found that matches DS in %s", domain, getParentDomain(domain))
		return false, fmt.Errorf("validation failed. No DNSKEY in %s found that matches DS in %s", domain, getParentDomain(domain))
	}
	return true, nil
}

func (s *Scan) ValidateDomain(domain string) (bool, error) {
	return s.validateDomain(domain)
}

func (s *Scan) validateDomain(domain string) (bool, error) {
	// TODO concurrency
	// get DNSKEY domain.
	// validate RRSIG on DNSKEY
	// get DS from parent.
	// create digest from DS based on digest from child
	// compare digest (parent) with child (RRSig digest)

	keyMap := make(map[uint16]*dns.DNSKEY)

	// get auth servers
	nsdata, err := s.FindNS(domain)
	if err != nil {
		log.Debugf("validateDomain() error: %#v", err)
	}
	for _, ns := range nsdata {
		for _, nsip := range ns.IP {
			var res structs.Response
			log.Debugf("Asking NS %s (%s) DNSKEY of %s", ns.Name, nsip.String(), domain)
			res, err = s.LookupDNSKEY(domain, nsip.String(), keyMap)
			if err != nil {
				return false, err
			}
			if res.Msg == nil {
				continue
			}
			valid, info, _ := validateDNSKEY(res.Msg.Answer)
			if valid {
				log.Debugf("RRSIG validated (%s -> %s)", time.Unix(info.Start, 0), time.Unix(info.End, 0))
			} else {
				log.Debugf("RRSIG not validated")
				return false, fmt.Errorf("validation failed. RRSIG on DNSKEY could not be validated by any DNSKEY for %s", domain)
			}
		}
	}
	log.Debugf("Found %v valid DNSKEY for %s", len(keyMap), domain)

	// get auth servers of parent
	log.Debugf("Finding NS of parent: %s", dns.Fqdn(getParentDomain(domain)))
	_, err = s.FindNS(getParentDomain(domain))
	if err != nil {
		log.Debugf("ValidateDomain() error: %#v", err)
	}

	// asking parent about DS
	return s.validateParentDS(domain, keyMap)
}
