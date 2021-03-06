package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/autoscaling"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/aws/aws-sdk-go/service/elb"
	"github.com/aws/aws-sdk-go/service/lambda"
)

var (
	AutoScaling = autoscaling.New(session.New(), nil)
	ECS         = ecs.New(session.New(), nil)
	ELB         = elb.New(session.New(), nil)
	Lambda      = lambda.New(session.New(), nil)
)

type Event struct {
	Records []Record
}

type Message struct {
	AutoScalingGroupName string
	EC2InstanceID        string
	LifecycleActionToken string
	LifecycleHookName    string
	LifecycleTransition  string
}

type Record struct {
	Sns struct {
		Message string
	}
}

type Metadata struct {
	Cluster string
	Rack    string
}

func main() {
	if len(os.Args) < 2 {
		die(fmt.Errorf("must specify event as argument"))
	}

	data := []byte(os.Args[1])

	var e Event

	if err := json.Unmarshal(data, &e); err != nil {
		die(err)
	}

	for _, r := range e.Records {
		if err := handle(r); err != nil {
			die(err)
		}
	}
}

func handle(r Record) error {
	var m Message

	if err := json.Unmarshal([]byte(r.Sns.Message), &m); err != nil {
		return err
	}

	fmt.Printf("m = %+v\n", m)

	if m.LifecycleTransition != "autoscaling:EC2_INSTANCE_TERMINATING" {
		return nil
	}

	md, err := metadata()
	if err != nil {
		return err
	}

	fmt.Printf("md = %+v\n", md)

	lbs, err := rackBalancers(md.Rack)
	if err != nil {
		return err
	}

	fmt.Printf("lbs = %+v\n", lbs)

	if err := deregisterInstanceFromLoadBalancers(lbs, m.EC2InstanceID); err != nil {
		return err
	}

	ci, err := containerInstance(md.Cluster, m.EC2InstanceID)
	if err != nil {
		return err
	}

	fmt.Printf("ci = %+v\n", ci)

	if err := deregisterClusterInstance(md.Cluster, ci); err != nil {
		return err
	}

	_, err = AutoScaling.CompleteLifecycleAction(&autoscaling.CompleteLifecycleActionInput{
		AutoScalingGroupName:  aws.String(m.AutoScalingGroupName),
		InstanceId:            aws.String(m.EC2InstanceID),
		LifecycleActionResult: aws.String("CONTINUE"),
		LifecycleActionToken:  aws.String(m.LifecycleActionToken),
		LifecycleHookName:     aws.String(m.LifecycleHookName),
	})
	if err != nil {
		return err
	}

	fmt.Println("success")

	return nil
}

func metadata() (*Metadata, error) {
	var md Metadata

	fres, err := Lambda.GetFunction(&lambda.GetFunctionInput{
		FunctionName: aws.String(os.Getenv("AWS_LAMBDA_FUNCTION_NAME")),
	})
	if err != nil {
		return nil, err
	}

	if err := json.Unmarshal([]byte(*fres.Configuration.Description), &md); err != nil {
		return nil, err
	}

	return &md, nil
}

func containerInstance(cluster string, id string) (string, error) {
	lreq := &ecs.ListContainerInstancesInput{
		Cluster:    aws.String(cluster),
		MaxResults: aws.Int64(10),
	}

	for {
		lres, err := ECS.ListContainerInstances(lreq)
		if err != nil {
			return "", err
		}

		dres, err := ECS.DescribeContainerInstances(&ecs.DescribeContainerInstancesInput{
			Cluster:            aws.String(cluster),
			ContainerInstances: lres.ContainerInstanceArns,
		})
		if err != nil {
			return "", err
		}

		for _, ci := range dres.ContainerInstances {
			if *ci.Ec2InstanceId == id {
				return *ci.ContainerInstanceArn, nil
			}
		}

		if lres.NextToken == nil {
			break
		}

		lreq.NextToken = lres.NextToken
	}

	return "", fmt.Errorf("could not find cluster instance: %s", id)
}

func deregisterClusterInstance(cluster, arn string) error {
	_, err := ECS.DeregisterContainerInstance(&ecs.DeregisterContainerInstanceInput{
		Cluster:           aws.String(cluster),
		ContainerInstance: aws.String(arn),
		Force:             aws.Bool(true),
	})
	if err != nil {
		return err
	}

	for {
		lreq := &ecs.ListServicesInput{
			Cluster:    aws.String(cluster),
			MaxResults: aws.Int64(10),
		}

		converged := true

		for {
			lres, err := ECS.ListServices(lreq)
			if err != nil {
				return err
			}

			dres, err := ECS.DescribeServices(&ecs.DescribeServicesInput{
				Cluster:  aws.String(cluster),
				Services: lres.ServiceArns,
			})
			if err != nil {
				return err
			}

			for _, s := range dres.Services {
				for _, d := range s.Deployments {
					fmt.Printf("service=%s running=%d pending=%d desired=%d\n", *s.ServiceArn, *d.RunningCount, *d.PendingCount, *d.DesiredCount)

					if *d.RunningCount != *d.DesiredCount {
						converged = false
					}
				}
			}

			if !converged {
				break
			}

			if lres.NextToken == nil {
				break
			}

			lreq.NextToken = lres.NextToken
		}

		if converged {
			fmt.Println("converged")
			return nil
		}

		time.Sleep(1 * time.Second)
	}

	return nil
}

func rackBalancers(rack string) ([]string, error) {
	breq := &elb.DescribeLoadBalancersInput{
		PageSize: aws.Int64(20),
	}

	lbs := []string{}

	for {
		bres, err := ELB.DescribeLoadBalancers(breq)
		if err != nil {
			return nil, err
		}

		names := []*string{}

		for _, lb := range bres.LoadBalancerDescriptions {
			names = append(names, lb.LoadBalancerName)
		}

		tres, err := ELB.DescribeTags(&elb.DescribeTagsInput{
			LoadBalancerNames: names,
		})
		if err != nil {
			return nil, err
		}

		for _, td := range tres.TagDescriptions {
			for _, t := range td.Tags {
				if *t.Key == "Rack" && *t.Value == rack {
					lbs = append(lbs, *td.LoadBalancerName)
				}
			}
		}

		if bres.NextMarker == nil {
			break
		}

		breq.Marker = bres.NextMarker
	}

	return lbs, nil
}

func deregisterInstanceFromLoadBalancers(lbs []string, instance string) error {
	ch := make(chan error)

	for _, lb := range lbs {
		go deregisterLoadBalancerInstance(lb, instance, ch)
	}

	for range lbs {
		if err := <-ch; err != nil {
			return err
		}
	}

	return nil
}

func deregisterLoadBalancerInstance(lb, instance string, ch chan error) {
	instances := []*elb.Instance{&elb.Instance{InstanceId: aws.String(instance)}}

	_, err := ELB.DeregisterInstancesFromLoadBalancer(&elb.DeregisterInstancesFromLoadBalancerInput{
		LoadBalancerName: aws.String(lb),
		Instances:        instances,
	})
	if err != nil {
		ch <- err
		return
	}

	for {
		res, err := ELB.DescribeInstanceHealth(&elb.DescribeInstanceHealthInput{
			LoadBalancerName: aws.String(lb),
			Instances:        instances,
		})

		fmt.Printf("res = %+v\n", res)
		fmt.Printf("err = %+v\n", err)

		if len(res.InstanceStates) < 1 {
			break
		}

		if *res.InstanceStates[0].State == "OutOfService" {
			break
		}

		time.Sleep(1 * time.Second)
	}

	ch <- nil
}

func die(err error) {
	fmt.Fprintf(os.Stderr, "ERROR: %s\n", err)
	os.Exit(1)
}
