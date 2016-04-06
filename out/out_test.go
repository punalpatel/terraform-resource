package main_test

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"runtime"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/awserr"
	"github.com/aws/aws-sdk-go/aws/credentials"
	awsSession "github.com/aws/aws-sdk-go/aws/session"
	awsec2 "github.com/aws/aws-sdk-go/service/ec2"
	"github.com/ljfranklin/terraform-resource/models"
	"github.com/ljfranklin/terraform-resource/test/helpers"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

var _ = Describe("Out", func() {

	var (
		outRequest  models.OutRequest
		ec2         *awsec2.EC2
		awsVerifier helpers.AWSVerifier
	)

	BeforeEach(func() {
		accessKey := os.Getenv("AWS_ACCESS_KEY")
		Expect(accessKey).ToNot(BeEmpty(), "AWS_ACCESS_KEY must be set")

		secretKey := os.Getenv("AWS_SECRET_KEY")
		Expect(secretKey).ToNot(BeEmpty(), "AWS_SECRET_KEY must be set")

		bucket := os.Getenv("AWS_BUCKET")
		Expect(bucket).ToNot(BeEmpty(), "AWS_BUCKET must be set")

		bucketPath := os.Getenv("AWS_BUCKET_PATH") // optional

		region := os.Getenv("AWS_REGION") // optional
		if region == "" {
			region = "us-east-1"
		}

		awsVerifier = helpers.AWSVerifier{
			AccessKey: accessKey,
			SecretKey: secretKey,
			Region:    region,
		}

		awsConfig := &aws.Config{
			Region:      aws.String(region),
			Credentials: credentials.NewStaticCredentials(accessKey, secretKey, ""),
		}
		ec2 = awsec2.New(awsSession.New(awsConfig))

		stateFileKey := path.Join(bucketPath, randomString("out-test"))

		outRequest = models.OutRequest{
			Source: models.Source{
				Bucket:          bucket,
				Key:             stateFileKey,
				AccessKeyID:     accessKey,
				SecretAccessKey: secretKey,
			},
			Params: models.Params{
				TerraformSource: "", // overridden in contexts
				TerraformVars: map[string]interface{}{
					"access_key": accessKey,
					"secret_key": secretKey,
				},
			},
		}
	})

	assertOutLifecycle := func() {
		It("succeeds in creating, outputing, and deleting infrastructure", func() {
			By("ensuring state file does not already exist")

			awsVerifier.ExpectS3FileToNotExist(
				outRequest.Source.Bucket,
				outRequest.Source.Key,
			)

			By("running 'out' to create an AWS VPC")

			pathToSources := getProjectRoot()
			command := exec.Command(pathToOutBinary, pathToSources)

			stdin, err := command.StdinPipe()
			Expect(err).ToNot(HaveOccurred())

			session, err := gexec.Start(command, GinkgoWriter, GinkgoWriter)
			Expect(err).ToNot(HaveOccurred())

			err = json.NewEncoder(stdin).Encode(outRequest)
			Expect(err).ToNot(HaveOccurred())
			stdin.Close()

			Eventually(session, 2*time.Minute).Should(gexec.Exit(0))

			By("ensuring that state file exists with valid version (LastModified)")

			awsVerifier.ExpectS3FileToExist(
				outRequest.Source.Bucket,
				outRequest.Source.Key,
			)

			actualOutput := models.OutResponse{}
			err = json.Unmarshal(session.Out.Contents(), &actualOutput)
			Expect(err).ToNot(HaveOccurred())

			// does version match format "2006-01-02T15:04:05Z"
			createVersion, err := time.Parse(time.RFC3339, actualOutput.Version.Version)
			Expect(err).ToNot(HaveOccurred())

			By("ensuring that output contains VPC ID and VPC exists")

			Expect(actualOutput.Metadata).ToNot(BeEmpty())
			vpcID := ""
			for _, field := range actualOutput.Metadata {
				if field.Name == "vpc_id" {
					vpcID = field.Value.(string)
					break
				}
			}
			Expect(vpcID).ToNot(BeEmpty())

			vpcParams := &awsec2.DescribeVpcsInput{
				VpcIds: []*string{
					aws.String(vpcID),
				},
			}
			resp, err := ec2.DescribeVpcs(vpcParams)
			Expect(err).ToNot(HaveOccurred())

			Expect(resp.Vpcs).To(HaveLen(1))
			Expect(*resp.Vpcs[0].VpcId).To(Equal(vpcID))

			By("running 'out' to update the VPC")

			outRequest.Params.TerraformVars["tag_name"] = "terraform-resource-test-updated"

			updateCommand := exec.Command(pathToOutBinary, pathToSources)

			updateStdin, err := updateCommand.StdinPipe()
			Expect(err).ToNot(HaveOccurred())

			updateSession, err := gexec.Start(updateCommand, GinkgoWriter, GinkgoWriter)
			Expect(err).ToNot(HaveOccurred())

			err = json.NewEncoder(updateStdin).Encode(outRequest)
			Expect(err).ToNot(HaveOccurred())
			stdin.Close()

			Eventually(updateSession, 2*time.Minute).Should(gexec.Exit(0))

			awsVerifier.ExpectS3FileToExist(
				outRequest.Source.Bucket,
				outRequest.Source.Key,
			)

			updateOutput := models.OutResponse{}
			err = json.Unmarshal(updateSession.Out.Contents(), &updateOutput)
			Expect(err).ToNot(HaveOccurred())

			updatedVersion, err := time.Parse(time.RFC3339, updateOutput.Version.Version)
			Expect(err).ToNot(HaveOccurred())
			Expect(updatedVersion).To(BeTemporally(">", createVersion))

			resp, err = ec2.DescribeVpcs(vpcParams)
			Expect(err).ToNot(HaveOccurred())

			Expect(resp.Vpcs).To(HaveLen(1))
			tags := resp.Vpcs[0].Tags
			Expect(tags).To(HaveLen(1))
			Expect(*tags[0].Key).To(Equal("Name"))
			Expect(*tags[0].Value).To(Equal("terraform-resource-test-updated"))

			By("running 'out' to delete the VPC")

			outRequest.Params.Action = models.DestroyAction
			deleteCommand := exec.Command(pathToOutBinary, pathToSources)

			deleteStdin, err := deleteCommand.StdinPipe()
			Expect(err).ToNot(HaveOccurred())

			deleteSession, err := gexec.Start(deleteCommand, GinkgoWriter, GinkgoWriter)
			Expect(err).ToNot(HaveOccurred())

			err = json.NewEncoder(deleteStdin).Encode(outRequest)
			Expect(err).ToNot(HaveOccurred())
			stdin.Close()

			Eventually(deleteSession, 2*time.Minute).Should(gexec.Exit(0))

			awsVerifier.ExpectS3FileToNotExist(
				outRequest.Source.Bucket,
				outRequest.Source.Key,
			)

			deleteOutput := models.OutResponse{}
			err = json.Unmarshal(deleteSession.Out.Contents(), &deleteOutput)
			Expect(err).ToNot(HaveOccurred())

			deletedVersion, err := time.Parse(time.RFC3339, deleteOutput.Version.Version)
			Expect(err).ToNot(HaveOccurred())
			Expect(deletedVersion).To(BeTemporally(">", updatedVersion))

			_, err = ec2.DescribeVpcs(vpcParams)
			Expect(err).To(HaveOccurred())
			ec2err := err.(awserr.Error)

			Expect(ec2err.Code()).To(Equal("InvalidVpcID.NotFound"))
		})
	}

	Context("when provided a local terraform source", func() {
		BeforeEach(func() {
			outRequest.Params.TerraformSource = "fixtures/aws/"
		})

		assertOutLifecycle()
	})

	Context("when provided a remote terraform source", func() {
		BeforeEach(func() {
			// changes to fixture must be pushed before running this test
			outRequest.Params.TerraformSource = "github.com/ljfranklin/terraform-resource//fixtures/aws/"
		})

		assertOutLifecycle()
	})
})

func getProjectRoot() string {
	_, filename, _, _ := runtime.Caller(1)
	return path.Join(path.Dir(filename), "..")
}

func randomString(prefix string) string {
	b := make([]byte, 4)
	_, err := rand.Read(b)
	Expect(err).ToNot(HaveOccurred())
	return fmt.Sprintf("%s-%x", prefix, b)
}
