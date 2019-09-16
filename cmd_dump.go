package ecsched

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudwatchevents"
	"github.com/aws/aws-sdk-go/service/ecs"
	"github.com/ghodss/yaml"
)

type cmdDump struct{}

func (cd *cmdDump) name() string {
	return "dump"
}

func (cd *cmdDump) description() string {
	return "dump tasks"
}

func (cd *cmdDump) run(ctx context.Context, argv []string, outStream, errStream io.Writer) error {
	fs := flag.NewFlagSet("ecsched dump", flag.ContinueOnError)
	fs.SetOutput(errStream)
	var (
		conf    = fs.String("conf", "", "configuration")
		region  = fs.String("region", "", "region")
		cluster = fs.String("cluster", "", "cluster")
		role    = fs.String("role", "", "role")
	)
	if err := fs.Parse(argv); err != nil {
		return err
	}
	a := getApp(ctx)
	c := a.Config
	accountID := a.AccountID
	if *conf != "" {
		f, err := os.Open(*conf)
		if err != nil {
			return err
		}
		defer f.Close()
		c, err = LoadConfig(f, a.AccountID)
		if err != nil {
			return err
		}
	}
	if c == nil {
		c = &Config{BaseConfig: &BaseConfig{}}
	}
	if *region == "" {
		*region = c.Region
	}
	if *cluster == "" {
		*cluster = c.Cluster
	}
	if c.Role == "" {
		c.Role = *role
	}
	if *role == "" {
		*role = c.Role
		if *role == "" {
			*role = defaultRole
		}
	}
	if *region == "" || *cluster == "" {
		return fmt.Errorf("region and cluster are must be specified")
	}
	c.Region = *region
	c.Cluster = *cluster

	sess := a.Session
	svc := cloudwatchevents.New(sess, &aws.Config{Region: region})
	ruleList, err := svc.ListRulesWithContext(ctx, &cloudwatchevents.ListRulesInput{})
	if err != nil {
		return err
	}
	var (
		rules            []*Rule
		ruleArnPrefix    = fmt.Sprintf("arn:aws:events:%s:%s:rule/", *region, accountID)
		clusterArn       = fmt.Sprintf("arn:aws:ecs:%s:%s:cluster/%s", *region, accountID, *cluster)
		taskDefArnPrefix = fmt.Sprintf("arn:aws:ecs:%s:%s:task-definition/", *region, accountID)
		roleArnPrefix    = fmt.Sprintf("arn:aws:iam::%s:role/", accountID)
		roleArn          = fmt.Sprintf("%s%s", roleArnPrefix, *role)
	)
RuleList:
	for _, r := range ruleList.Rules {
		if !strings.HasPrefix(*r.Arn, ruleArnPrefix) {
			continue
		}
		ta, err := svc.ListTargetsByRuleWithContext(ctx, &cloudwatchevents.ListTargetsByRuleInput{
			Rule: r.Name,
		})
		if err != nil {
			return err
		}
		var targets []*Target
		for _, t := range ta.Targets {
			if *t.Arn != clusterArn {
				continue RuleList
			}
			targetID := *t.Id
			if targetID == *r.Name {
				targetID = ""
			}
			ecsParams := t.EcsParameters
			if ecsParams == nil {
				// ignore rule which have some non ecs targets
				continue RuleList
			}
			target := &Target{TargetID: targetID}

			if role := *t.RoleArn; role != roleArn {
				target.Role = strings.TrimPrefix(role, roleArnPrefix)
			}

			taskCount := *ecsParams.TaskCount
			if taskCount != 1 {
				target.TaskCount = taskCount
			}
			target.TaskDefinition = strings.TrimPrefix(*ecsParams.TaskDefinitionArn, taskDefArnPrefix)

			taskOv := &ecs.TaskOverride{}
			if err := json.Unmarshal([]byte(*t.Input), taskOv); err != nil {
				return err
			}
			var contOverrides []*ContainerOverride
			for _, co := range taskOv.ContainerOverrides {
				var cmd []string
				for _, c := range co.Command {
					cmd = append(cmd, *c)
				}
				env := map[string]string{}
				for _, kv := range co.Environment {
					env[*kv.Name] = *kv.Value
				}
				contOverrides = append(contOverrides, &ContainerOverride{
					Name:        *r.Name,
					Command:     cmd,
					Environment: env,
				})
			}
			target.ContainerOverrides = contOverrides
			targets = append(targets, target)
		}
		ru := &Rule{
			Name:               *r.Name,
			Description:        *r.Description,
			ScheduleExpression: *r.ScheduleExpression,
			Disabled:           *r.State == "DISABLED",
		}
		switch len(targets) {
		case 0:
			continue RuleList
		case 1:
			ru.Target = targets[0]
		default:
			// not supported multiple target yet
			continue RuleList
			// ru.Target = targets[0]
			// ru.Targets = targets[1:]
		}
		rules = append(rules, ru)
	}
	c.Rules = rules
	bs, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	fmt.Fprint(outStream, string(bs))
	return nil
}