package pdsql

import (
	"net"
	"strconv"
	"strings"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/request"
	"github.com/jinzhu/gorm"
	"github.com/miekg/dns"
	pdnsmodel "github.com/xiaosier/wps/pdns/model"
	"golang.org/x/net/context"
)

const Name = "pdsql"
const Debug = true
const LogPath = "/data0/logs/"
const logPrefix = "pdsql"

type PowerDNSGenericSQLBackend struct {
	*gorm.DB
	Debug          bool
	Next           plugin.Handler
	Log            *BLogs
	LogInitSuccess bool
}

func (self PowerDNSGenericSQLBackend) Name() string { return Name }

func (self PowerDNSGenericSQLBackend) InitLogs() {
	var logLevel int
	if Debug {
		logLevel = DEBUG
	} else {
		logLevel = BAK
	}
	logs, err := NewLogs(LogPath, logPrefix, logLevel, true)
	if err != nil {
		self.LogInitSuccess = false
		return
	} else {
		self.LogInitSuccess = true
	}
	self.Log = logs
}

func (self PowerDNSGenericSQLBackend) WriteLog(logInterface string, format string, a ...interface{}) {
	if !self.LogInitSuccess {
		// log not init success
		return
	}
	switch logInterface {
	case "Info":
		self.Log.Info(format, a)
	case "Debug":
		self.Log.Debug(format, a)
	case "Warn":
		self.Log.Warn(format, a)
	case "Error":
		self.Log.Error(format, a)
	}
}

func (self PowerDNSGenericSQLBackend) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}
	// start log
	self.InitLogs()
	a := new(dns.Msg)
	a.SetReply(r)
	a.Compress = true
	a.Authoritative = true

	var records []*pdnsmodel.Record
	query := pdnsmodel.Record{Name: state.QName(), Type: state.Type(), Disabled: false}
	if query.Name != "." {
		// remove last dot
		query.Name = query.Name[:len(query.Name)-1]
	}

	switch state.QType() {
	case dns.TypeANY:
		query.Type = ""
	}

	var checkRecord = false

	if err := self.Where(query).Find(&records).Error; err != nil {
		self.WriteLog("Info", "can not find record, detail: ", err.Error())
		for {
			if err == gorm.ErrRecordNotFound && state.Type() == "A" {
				// if can not find A record, go to find CNAME record
				query.Type = "CNAME"
				if self.Where(query).Find(&records).Error == nil {
					checkRecord = true
					break
				}
			}
			if err == gorm.ErrRecordNotFound {
				query.Type = "SOA"
				if self.Where(query).Find(&records).Error == nil {
					rr := new(dns.SOA)
					rr.Hdr = dns.RR_Header{Name: state.QName(), Rrtype: dns.TypeSOA, Class: state.QClass()}
					if ParseSOA(rr, records[0].Content) {
						a.Extra = append(a.Extra, rr)
					}
				}
				break
			} else {
				return dns.RcodeServerFailure, err
			}
		}
	} else {
		self.WriteLog("Info", "called")
		self.WriteLog("Info", "record length", len(records))
		checkRecord = true
	}
	if checkRecord {
		if len(records) == 0 {
			var err error
			records, err = self.SearchWildcard(state.QName(), state.QType())
			if err != nil {
				return dns.RcodeServerFailure, err
			}
		}
		for _, v := range records {
			typ := dns.StringToType[v.Type]
			hrd := dns.RR_Header{Name: state.QName(), Rrtype: typ, Class: state.QClass(), Ttl: v.Ttl}
			if !strings.HasSuffix(hrd.Name, ".") {
				hrd.Name += "."
			}
			rr := dns.TypeToRR[typ]()

			// todo support more type
			// this is enough for most query
			switch rr := rr.(type) {
			case *dns.SOA:
				rr.Hdr = hrd
				if !ParseSOA(rr, v.Content) {
					rr = nil
				}
			case *dns.A:
				rr.Hdr = hrd
				rr.A = net.ParseIP(v.Content)
			case *dns.AAAA:
				rr.Hdr = hrd
				rr.AAAA = net.ParseIP(v.Content)
			case *dns.TXT:
				rr.Hdr = hrd
				rr.Txt = []string{v.Content}
			case *dns.NS:
				rr.Hdr = hrd
				rr.Ns = v.Content
			case *dns.PTR:
				rr.Hdr = hrd
				// pdns don't need the dot but when we answer, we need it
				if strings.HasSuffix(v.Content, ".") {
					rr.Ptr = v.Content
				} else {
					rr.Ptr = v.Content + "."
				}
			case *dns.CNAME:
				rr.Hdr = hrd
				if strings.HasSuffix(v.Content, ".") {
					rr.Target = v.Content
				} else {
					rr.Target = v.Content + "."
				}
			default:
				// drop unsupported
			}

			a.Answer = append(a.Answer, rr)
		}
	}
	if len(a.Answer) == 0 {
		return plugin.NextOrFailure(self.Name(), self.Next, ctx, w, r)
	}

	return 0, w.WriteMsg(a)
}

func (self PowerDNSGenericSQLBackend) SearchWildcard(qname string, qtype uint16) (redords []*pdnsmodel.Record, err error) {
	// find domain, then find matched sub domain
	name := qname
	qnameNoDot := qname[:len(qname)-1]
	typ := dns.TypeToString[qtype]
	name = qnameNoDot
NEXT_ZONE:
	if i := strings.IndexRune(name, '.'); i > 0 {
		name = name[i+1:]
	} else {
		return
	}
	var domain pdnsmodel.Domain

	if err := self.Limit(1).Find(&domain, "name = ?", name).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			goto NEXT_ZONE
		}
		return nil, err
	}

	if err := self.Find(&redords, "domain_id = ? and ( ? = 'ANY' or type = ? ) and name like '%*%'", domain.ID, typ, typ).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	// filter
	var matched []*pdnsmodel.Record
	for _, v := range redords {
		if WildcardMatch(qnameNoDot, v.Name) {
			matched = append(matched, v)
		}
	}
	redords = matched
	return
}

func ParseSOA(rr *dns.SOA, line string) bool {
	splites := strings.Split(line, " ")
	if len(splites) < 7 {
		return false
	}
	rr.Ns = splites[0]
	rr.Mbox = splites[1]
	if i, err := strconv.Atoi(splites[2]); err != nil {
		return false
	} else {
		rr.Serial = uint32(i)
	}
	if i, err := strconv.Atoi(splites[3]); err != nil {
		return false
	} else {
		rr.Refresh = uint32(i)
	}
	if i, err := strconv.Atoi(splites[4]); err != nil {
		return false
	} else {
		rr.Retry = uint32(i)
	}
	if i, err := strconv.Atoi(splites[5]); err != nil {
		return false
	} else {
		rr.Expire = uint32(i)
	}
	if i, err := strconv.Atoi(splites[6]); err != nil {
		return false
	} else {
		rr.Minttl = uint32(i)
	}
	return true
}

// Dummy wildcard match
func WildcardMatch(s1, s2 string) bool {
	if s1 == "." || s2 == "." {
		return true
	}

	l1 := dns.SplitDomainName(s1)
	l2 := dns.SplitDomainName(s2)

	if len(l1) != len(l2) {
		return false
	}

	for i := range l1 {
		if !equal(l1[i], l2[i]) {
			return false
		}
	}

	return true
}

func equal(a, b string) bool {
	if b == "*" || a == "*" {
		return true
	}
	// might be lifted into API function.
	la := len(a)
	lb := len(b)
	if la != lb {
		return false
	}

	for i := la - 1; i >= 0; i-- {
		ai := a[i]
		bi := b[i]
		if ai >= 'A' && ai <= 'Z' {
			ai |= 'a' - 'A'
		}
		if bi >= 'A' && bi <= 'Z' {
			bi |= 'a' - 'A'
		}
		if ai != bi {
			return false
		}
	}
	return true
}
