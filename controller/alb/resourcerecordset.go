package alb

import (
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/service/elbv2"
	"github.com/aws/aws-sdk-go/service/route53"
	"github.com/coreos/alb-ingress-controller/awsutil"
	"github.com/coreos/alb-ingress-controller/log"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	// Amount of time, in seconds, between each attempt to validate a created or modified record's
	// status has reached insyncR53DNSStatus state.
	validateSleepDuration int = 10
	// Maximum attempts should be made to validate a created or modified resource record set has
	// reached insyncR53DNSStatus state.
	maxValidateRecordAttempts int = 30
	// Status used to signify that resource record set that the changes have replicated to all Amazon
	// Route 53 DNS servers.
	insyncR53DNSStatus string = "INSYNC"
)

// ResourceRecordSet contains the relevant Route 53 zone id for the host name along with the
// current and desired state.
type ResourceRecordSet struct {
	IngressID                *string
	ZoneID                   *string
	CurrentResourceRecordSet *route53.ResourceRecordSet
	DesiredResourceRecordSet *route53.ResourceRecordSet
}

// NewResourceRecordSet returns a new route53.ResourceRecordSet based on the LoadBalancer provided.
func NewResourceRecordSet(hostname *string, ingressID *string) (*ResourceRecordSet, error) {
	zoneID, err := awsutil.Route53svc.GetZoneID(hostname)
	if err != nil {
		log.Errorf("Unabled to locate ZoneId for %s.", *ingressID, hostname)
		return nil, err
	}

	name := *hostname
	if !strings.HasPrefix(*hostname, ".") {
		name = *hostname + "."
	}
	record := &ResourceRecordSet{
		DesiredResourceRecordSet: &route53.ResourceRecordSet{
			AliasTarget: &route53.AliasTarget{
				EvaluateTargetHealth: aws.Bool(false),
			},
			Name: aws.String(name),
			Type: aws.String("A"),
		},
		ZoneID:    zoneID.Id,
		IngressID: ingressID,
	}

	return record, nil
}

// SyncState compares the current and desired state of this ResourceRecordSet instance. Comparison
// results in no action, the creation, the deletion, or the modification of Route 53 resource
// record set to satisfy the ingress's current state.
func (r *ResourceRecordSet) SyncState(lb *LoadBalancer) *ResourceRecordSet {
	switch {
	// No DesiredState means record should be deleted.
	case r.DesiredResourceRecordSet == nil:
		log.Infof("Start Route53 resource record set deletion.", *r.IngressID)
		r.delete(lb)

	// No CurrentState means record doesn't exist in AWS and should be created.
	case r.CurrentResourceRecordSet == nil:
		log.Infof("Start Route53 resource record set creation.", *r.IngressID)
		r.PopulateFromLoadBalancer(lb.CurrentLoadBalancer)
		r.create(lb)

	// Current and Desired exist and need for modification should be evaluated.
	default:
		r.PopulateFromLoadBalancer(lb.CurrentLoadBalancer)
		// Only perform modifictation if needed.
		if r.needsModification() {
			log.Infof("Start Route 53 resource record set modification.", *r.IngressID)
			r.modify(lb)
		} else {
			log.Debugf("No modification of Route 53 resource record set required.", *r.IngressID)
		}
	}

	return r
}

func (r *ResourceRecordSet) create(lb *LoadBalancer) error {
	// If a record pre-exists, delete it.
	existing := awsutil.LookupExistingRecord(lb.Hostname)
	if existing != nil {
		if *existing.Type != route53.RRTypeA {
			r.CurrentResourceRecordSet = existing
			r.delete(lb)
		}
	}

	err := r.modify(lb)
	if err != nil {
		log.Infof("Failed Route 53 resource record set creation. DNS: %s | Type: %s | Target: %s | Error: %s.",
			*lb.IngressID, *lb.Hostname, *r.CurrentResourceRecordSet.Type, log.Prettify(*r.CurrentResourceRecordSet.AliasTarget), err.Error())
		return err
	}

	log.Infof("Completed Route 53 resource record set creation. DNS: %s | Type: %s | Target: %s.",
		*lb.IngressID, *lb.Hostname, *r.CurrentResourceRecordSet.Type, log.Prettify(*r.CurrentResourceRecordSet.AliasTarget))
	return nil
}

func (r *ResourceRecordSet) delete(lb *LoadBalancer) error {
	// Attempt record deletion
	params := &route53.ChangeResourceRecordSetsInput{
		ChangeBatch: &route53.ChangeBatch{
			Changes: []*route53.Change{
				{
					Action:            aws.String("DELETE"),
					ResourceRecordSet: r.CurrentResourceRecordSet,
				},
			},
		},
		HostedZoneId: r.ZoneID,
	}

	if r.CurrentResourceRecordSet == nil {
		return nil
	}

	_, err := awsutil.Route53svc.Svc.ChangeResourceRecordSets(params)
	if err != nil {
		// When record is invalid change batch, resource did not exist in state expected. This is likely
		// caused by the record having already been deleted. In this case, return nil, as there is no
		// work to be done.
		awsErr := err.(awserr.Error)
		if awsErr.Code() == route53.ErrCodeInvalidChangeBatch {
			log.Warnf("Cancelled deletion of Route 53 resource record set as state of record did not exist in Route 53 as expected. This could mean the record was already deleted.",
				*r.IngressID)
			return nil
		}
		log.Errorf("Failed deletion of route53 resource record set. DNS: %s | Target: %s | Error: %s",
			*r.IngressID, *r.CurrentResourceRecordSet.Name, log.Prettify(*r.CurrentResourceRecordSet.AliasTarget), err.Error())
		return err
	}

	log.Infof("Completed deletion of Route 53 resource record set. DNS: %s | Type: %s | Target: %s.",
		*lb.IngressID, *lb.Hostname, *r.CurrentResourceRecordSet.Type, log.Prettify(*r.CurrentResourceRecordSet.AliasTarget))
	r.CurrentResourceRecordSet = nil
	return nil
}

func (r *ResourceRecordSet) modify(lb *LoadBalancer) error {
	// Use all values from DesiredResourceRecordSet to run upsert against existing RecordSet in AWS.
	params := &route53.ChangeResourceRecordSetsInput{
		ChangeBatch: &route53.ChangeBatch{
			Changes: []*route53.Change{
				{
					Action: aws.String(route53.ChangeActionUpsert),
					ResourceRecordSet: &route53.ResourceRecordSet{
						Name:        r.DesiredResourceRecordSet.Name,
						Type:        r.DesiredResourceRecordSet.Type,
						AliasTarget: r.DesiredResourceRecordSet.AliasTarget,
					},
				},
			},
			Comment: aws.String("Managed by Kubernetes"),
		},
		HostedZoneId: r.ZoneID, // Required
	}

	resp, err := awsutil.Route53svc.Svc.ChangeResourceRecordSets(params)
	if err != nil {
		awsutil.AWSErrorCount.With(prometheus.Labels{"service": "Route53", "request": "ChangeResourceRecordSets"}).Add(float64(1))
		log.Errorf("Failed Route 53 resource record set modification. UPSERT to AWS API failed. Error: %s",
			*r.IngressID, err.Error())
		return err
	}

	// Verify that resource record set was created successfully, otherwise return error.
	success := r.verifyRecordCreated(*resp.ChangeInfo.Id)
	if !success {
		log.Errorf("Failed Route 53 resource record set modification. Unable to verify DNS propagation. DNS: %s | Type: %s | AliasTarget: %s",
			*r.IngressID, *r.DesiredResourceRecordSet.Name, *r.DesiredResourceRecordSet.Type, log.Prettify(*r.DesiredResourceRecordSet.AliasTarget))
		return fmt.Errorf("ResourceRecordSet %s never validated", *r.DesiredResourceRecordSet.Name)
	}

	// When delete is required, delete the CurrentResourceRecordSet.
	deleteRequired := r.isDeleteRequired()
	if deleteRequired {
		r.delete(lb)
	}

	// Upon success, ensure all possible updated attributes are updated in local Resource Record Set reference
	r.CurrentResourceRecordSet = r.DesiredResourceRecordSet
	r.DesiredResourceRecordSet = nil
	log.Infof("Completed Route 53 resource record set modification. DNS: %s | Type: %s | AliasTarget: %s",
		*r.IngressID, *r.CurrentResourceRecordSet.Name, *r.CurrentResourceRecordSet.Type, log.Prettify(*r.CurrentResourceRecordSet.AliasTarget))

	return nil
}

// Verify the ResourceRecordSet's desired state has been setup and reached RUNNING status.
func (r *ResourceRecordSet) verifyRecordCreated(changeID string) bool {
	created := false

	// Attempt to verify the existence of the deisred Resource Record Set up to as many times defined in
	// maxValidateRecordAttempts
	for i := 0; i < maxValidateRecordAttempts; i++ {
		time.Sleep(time.Duration(validateSleepDuration) * time.Second)
		params := &route53.GetChangeInput{
			Id: &changeID,
		}
		resp, _ := awsutil.Route53svc.Svc.GetChange(params)
		status := *resp.ChangeInfo.Status
		if status != insyncR53DNSStatus {
			// Record does not exist, loop again.
			log.Infof("%s status was %s in Route 53. Attempt %d/%d.", *r.IngressID, *r.DesiredResourceRecordSet.Name,
				status, i+1, maxValidateRecordAttempts)
			continue
		}
		// Record located. Set created to true and break from loop
		created = true
		break
	}

	return created
}

// Checks to see if the CurrentResourceRecordSet exists and whether its hostname differs from
// DesiredResourceRecordSet's hostname. If both are true, the CurrentResourceRecordSet will still
// exist in AWS and should be deleted. In that case, this method returns true.
func (r *ResourceRecordSet) isDeleteRequired() bool {
	if r.CurrentResourceRecordSet == nil {
		return false
	}
	if *r.CurrentResourceRecordSet.Name == *r.DesiredResourceRecordSet.Name {
		return false
	}
	// The ResourceRecordSet DNS name has changed between desired and current and should be deleted.
	return true
}

// Determine whether there is a difference between CurrentResourceRecordSet and DesiredResourceRecordSet that requires
// a modification. Checks for whether hostname, record type, load balancer alias target (host name), and/or load
// balancer alias target's hosted zone are different.
func (r *ResourceRecordSet) needsModification() bool {
	switch {
	// No resource record set currently exists; modification required.
	case r.CurrentResourceRecordSet == nil:
		return true
	// not sure if we need both conditions here.
	// Hostname has changed; modification required.
	case *r.CurrentResourceRecordSet.Name != *r.DesiredResourceRecordSet.Name:
		return true
	// Load balancer's hostname has changed; modification required.
	case *r.CurrentResourceRecordSet.AliasTarget.DNSName != *r.DesiredResourceRecordSet.AliasTarget.DNSName:
		return true
	// DNS record's resource type has changed; modification required.
	case *r.CurrentResourceRecordSet.Type != *r.DesiredResourceRecordSet.Type:
		return true
	// Load balancer's dns hosted zone has changed; modification required.
	case *r.CurrentResourceRecordSet.AliasTarget.HostedZoneId != *r.DesiredResourceRecordSet.AliasTarget.HostedZoneId:
		return true
	}
	return false
}

// PopulateFromLoadBalancer configures the DesiredResourceRecordSet with values from a n elbv2.LoadBalancer
func (r *ResourceRecordSet) PopulateFromLoadBalancer(lb *elbv2.LoadBalancer) {
	r.DesiredResourceRecordSet.AliasTarget.DNSName = aws.String(*lb.DNSName + ".")
	r.DesiredResourceRecordSet.AliasTarget.HostedZoneId = lb.CanonicalHostedZoneId
}
