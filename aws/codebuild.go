package aws

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BishopFox/cloudfox/aws/sdk"
	"github.com/BishopFox/cloudfox/internal"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/bishopfox/awsservicemap"
	"github.com/sirupsen/logrus"
)

type CodeBuildModule struct {
	// General configuration data
	CodeBuildClient sdk.CodeBuildClientInterface
	IAMClient       sdk.AWSIAMClientInterface

	Caller              sts.GetCallerIdentityOutput
	AWSRegions          []string
	AWSOutputType       string
	AWSTableCols        string
	PmapperDataBasePath string

	Goroutines     int
	AWSProfile     string
	SkipAdminCheck bool
	WrapTable      bool
	pmapperMod     PmapperModule
	pmapperError   error
	iamSimClient   IamSimulatorModule

	// Main module data
	Projects       []Project
	CommandCounter internal.CommandCounter
	// Used to store output data for pretty printing
	output internal.OutputData2
	modLog *logrus.Entry
}

type Project struct {
	Region     string
	Name       string
	Arn        string
	Role       string
	Admin      string
	CanPrivEsc string
}

func (m *CodeBuildModule) PrintCodeBuildProjects(outputDirectory string, verbosity int) {
	// These struct values are used by the output module
	m.output.Verbosity = verbosity
	m.output.Directory = outputDirectory
	m.output.CallingModule = "codebuild"
	localAdminMap := make(map[string]bool)

	m.modLog = internal.TxtLog.WithFields(logrus.Fields{
		"module": m.output.CallingModule,
	})
	if m.AWSProfile == "" {
		m.AWSProfile = internal.BuildAWSPath(m.Caller)
	}

	fmt.Printf("[%s][%s] Enumerating CodeBuild projects for account %s.\n", cyan(m.output.CallingModule), cyan(m.AWSProfile), aws.ToString(m.Caller.Account))
	m.pmapperMod, m.pmapperError = InitPmapperGraph(m.Caller, m.AWSProfile, m.Goroutines, m.PmapperDataBasePath)
	m.iamSimClient = InitIamCommandClient(m.IAMClient, m.Caller, m.AWSProfile, m.Goroutines)

	wg := new(sync.WaitGroup)
	semaphore := make(chan struct{}, m.Goroutines)

	// Create a channel to signal the spinner aka task status goroutine to finish
	spinnerDone := make(chan bool)
	//fire up the the task status spinner/updated
	go internal.SpinUntil(m.output.CallingModule, &m.CommandCounter, spinnerDone, "regions")

	//create a channel to receive the objects
	dataReceiver := make(chan Project)

	// Create a channel to signal to stop
	receiverDone := make(chan bool)

	go m.Receiver(dataReceiver, receiverDone)

	for _, region := range m.AWSRegions {
		wg.Add(1)
		m.CommandCounter.Pending++
		go m.executeChecks(region, wg, semaphore, dataReceiver)

	}

	wg.Wait()

	// Send a message to the spinner goroutine to close the channel and stop
	spinnerDone <- true
	<-spinnerDone
	receiverDone <- true
	<-receiverDone

	// Perform role analysis
	if m.pmapperError == nil {
		for i := range m.Projects {
			m.Projects[i].Admin, m.Projects[i].CanPrivEsc = GetPmapperResults(m.SkipAdminCheck, m.pmapperMod, &m.Projects[i].Role)
		}
	} else {
		for i := range m.Projects {
			m.Projects[i].Admin, m.Projects[i].CanPrivEsc = GetIamSimResult(m.SkipAdminCheck, &m.Projects[i].Role, m.iamSimClient, localAdminMap)
		}
	}

	// add - if struct is not empty do this. otherwise, dont write anything.
	m.output.Headers = []string{
		"Account",
		"Region",
		"Name",
		"Role",
		"IsAdminRole?",
		"CanPrivEscToAdmin?",
	}

	// If the user specified table columns, use those.
	// If the user specified -o wide, use the wide default cols for this module.
	// Otherwise, use the hardcoded default cols for this module.
	var tableCols []string
	// If the user specified table columns, use those.
	if m.AWSTableCols != "" {
		// If the user specified wide as the output format, use these columns.
		// remove any spaces between any commas and the first letter after the commas
		m.AWSTableCols = strings.ReplaceAll(m.AWSTableCols, ", ", ",")
		m.AWSTableCols = strings.ReplaceAll(m.AWSTableCols, ",  ", ",")
		tableCols = strings.Split(m.AWSTableCols, ",")
	} else if m.AWSOutputType == "wide" {
		tableCols = []string{
			"Account",
			"Region",
			"Name",
			"Role",
			"IsAdminRole?",
			"CanPrivEscToAdmin?",
		}
		// Otherwise, use the default columns.
	} else {
		tableCols = []string{
			"Region",
			"Name",
			"Role",
			"IsAdminRole?",
			"CanPrivEscToAdmin?",
		}
	}

	// Remove the pmapper row if there is no pmapper data
	if m.pmapperError != nil {
		sharedLogger.Errorf("%s - %s - No pmapper data found for this account. Skipping the pmapper column in the output table.", m.output.CallingModule, m.AWSProfile)
		tableCols = removeStringFromSlice(tableCols, "CanPrivEscToAdmin?")
	}

	// Table rows

	for i := range m.Projects {
		m.output.Body = append(
			m.output.Body,
			[]string{
				aws.ToString(m.Caller.Account),
				m.Projects[i].Region,
				m.Projects[i].Name,
				m.Projects[i].Role,
				m.Projects[i].Admin,
				m.Projects[i].CanPrivEsc,
			},
		)
	}

	var seen []string
	for _, project := range m.Projects {
		if !internal.Contains(project.Name, seen) {
			seen = append(seen, project.Name)
		}
	}

	if len(m.output.Body) > 0 {
		m.output.FilePath = filepath.Join(outputDirectory, "cloudfox-output", "aws", fmt.Sprintf("%s-%s", m.AWSProfile, aws.ToString(m.Caller.Account)))
		o := internal.OutputClient{
			Verbosity:     verbosity,
			CallingModule: m.output.CallingModule,
			Table: internal.TableClient{
				Wrap: m.WrapTable,
			},
		}
		o.Table.TableFiles = append(o.Table.TableFiles, internal.TableFile{
			Header:    m.output.Headers,
			Body:      m.output.Body,
			TableCols: tableCols,
			Name:      m.output.CallingModule,
		})
		o.PrefixIdentifier = m.AWSProfile
		o.Table.DirectoryName = filepath.Join(outputDirectory, "cloudfox-output", "aws", fmt.Sprintf("%s-%s", m.AWSProfile, aws.ToString(m.Caller.Account)))
		o.WriteFullOutput(o.Table.TableFiles, nil)
		//m.writeLoot(o.Table.DirectoryName, verbosity)
		fmt.Printf("[%s][%s] %d projects found.\n", cyan(m.output.CallingModule), cyan(m.AWSProfile), len(m.output.Body))
		fmt.Printf("[%s][%s] For context and next steps: https://github.com/BishopFox/cloudfox/wiki/AWS-Commands#%s\n", cyan(m.output.CallingModule), cyan(m.AWSProfile), m.output.CallingModule)

	} else {
		fmt.Printf("[%s][%s] No projects found, skipping the creation of an output file.\n", cyan(m.output.CallingModule), cyan(m.AWSProfile))
	}

}

func (m *CodeBuildModule) executeChecks(r string, wg *sync.WaitGroup, semaphore chan struct{}, dataReceiver chan Project) {
	defer wg.Done()

	servicemap := &awsservicemap.AwsServiceMap{
		JsonFileSource: "DOWNLOAD_FROM_AWS",
	}
	res, err := servicemap.IsServiceInRegion("codebuild", r)
	if err != nil {
		m.modLog.Error(err)
	}
	if res {
		m.CommandCounter.Total++
		wg.Add(1)
		m.getcodeBuildProjectsPerRegion(r, wg, semaphore, dataReceiver)
	}
}

func (m *CodeBuildModule) Receiver(receiver chan Project, receiverDone chan bool) {
	defer close(receiverDone)
	for {
		select {
		case data := <-receiver:
			m.Projects = append(m.Projects, data)
		case <-receiverDone:
			receiverDone <- true
			return
		}
	}
}

func (m *CodeBuildModule) getcodeBuildProjectsPerRegion(r string, wg *sync.WaitGroup, semaphore chan struct{}, dataReceiver chan Project) {
	defer wg.Done()

	semaphore <- struct{}{}
	defer func() { <-semaphore }()

	projects, err := sdk.CachedCodeBuildListProjects(m.CodeBuildClient, aws.ToString(m.Caller.Account), r)
	if err != nil {
		sharedLogger.Error(err.Error())
	}

	for _, project := range projects {
		details, err := sdk.CachedCodeBuildBatchGetProjects(m.CodeBuildClient, aws.ToString(m.Caller.Account), r, project)
		if err != nil {
			sharedLogger.Error(err.Error())
		}

		dataReceiver <- Project{
			Name:       aws.ToString(details.Name),
			Region:     r,
			Role:       aws.ToString(details.ServiceRole),
			Admin:      "",
			CanPrivEsc: "",
		}

	}

}
