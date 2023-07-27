#!/usr/bin/env python3
################################################################
#Name: Simple iperf3  test
#Desc: create 2 kind clusters :
# 1) MBG and iperf3 client
# 2) MBG and iperf3 server    
###############################################################
import os,time
import subprocess as sp
import sys
import argparse


proj_dir = os.path.dirname(os.path.dirname(os.path.dirname(os.path.dirname( os.path.abspath(__file__)))))
sys.path.insert(0,f'{proj_dir}')

from tests.utils.mbgAux import runcmd, runcmdb, printHeader, waitPod, getPodName, getMbgPorts,buildMbg,buildMbgctl,getPodIp,getPodNameIp
from tests.iperf3.kind.connect_mbgs import connectMbgs
from tests.iperf3.kind.iperf3_service_create import setIperf3client, setIperf3Server
from tests.iperf3.kind.iperf3_service_import import importService
from tests.iperf3.kind.iperf3_service_get import getService
from tests.iperf3.kind.iperf3_client_start import directTestIperf3,testIperf3Client
from tests.iperf3.kind.apply_policy import addPolicy

from tests.utils.kind.kindAux import useKindCluster, getKindIp,startKindClusterMbg

############################### MAIN ##########################
if __name__ == "__main__":
    parser = argparse.ArgumentParser(description='Description of your program')
    parser.add_argument('-d','--dataplane', help='choose which dataplane to use mtls/tcp', required=False, default="mtls")
    parser.add_argument('-c','--cni', help='Which cni to use default(kindnet)/flannel/calico/diff (different cni for each cluster)', required=False, default="default")
    parser.add_argument('-z','--zeroTrust', help='use zerotrust by default or not', required=False, default=False)

    args = vars(parser.parse_args())

    printHeader("\n\nStart Kind Test\n\n")
    printHeader("Start pre-setting")

    dataplane = args["dataplane"]
    cni = args["cni"]
    zeroTrust= args["zeroTrust"]
    #MBG1 parameters 
    mbg1DataPort    = "30001"
    mbg1cPort       = "30443"
    mbg1cPortLocal  = 443
    mbg1crtFlags    = f"--certca ./mtls/ca.crt --cert ./mtls/mbg1.crt --key ./mtls/mbg1.key"  if dataplane =="mtls" else ""
    mbg1Name        = "mbg1"
    gwctl1Name     = "gwctl1"
    mbg1cni         = cni 
    srcSvc          = "iperf3-client"

    
    #MBG2 parameters 
    mbg2DataPort    = "30001"
    mbg2cPort       = "30443"
    mbg2cPortLocal  = 443
    mbg2crtFlags    = f"--certca ./mtls/ca.crt --cert ./mtls/mbg2.crt --key ./mtls/mbg2.key"  if dataplane =="mtls" else ""
    mbg2Name        = "mbg2"
    gwctl2Name     = "gwctl2"
    mbg2cni         = "flannel" if cni == "diff" else cni
    destSvc         = "iperf3-server"
    destPort        = 5000
    kindDestPort    = "30001"
    
        
    #folders
    folCl=f"{proj_dir}/tests/iperf3/manifests/iperf3-client"
    folSv=f"{proj_dir}/tests/iperf3/manifests/iperf3-server"
    
    print(f'Working directory {proj_dir}')
    os.chdir(proj_dir)
    
    ### clean 
    print(f"Clean old kinds")
    os.system("make clean-kind-iperf3")
    
    ### build docker environment 
    printHeader(f"Build docker image")
    os.system("make docker-build")
    
    
    ### Build MBG in Kind clusters environment 
    startKindClusterMbg(mbg1Name, gwctl1Name, mbg1cPortLocal, mbg1cPort, mbg1DataPort, dataplane ,mbg1crtFlags, cni=mbg1cni, zeroTrust=zeroTrust)        
    startKindClusterMbg(mbg2Name, gwctl2Name, mbg2cPortLocal, mbg2cPort, mbg2DataPort, dataplane ,mbg2crtFlags, cni=mbg2cni, zeroTrust=zeroTrust)        
      
    ###get mbg parameters
    useKindCluster(mbg1Name)
    mbg1Pod, _           = getPodNameIp("mbg")
    mbg1Ip               = getKindIp("mbg1")
    gwctl1Pod, gwctl1Ip= getPodNameIp("gwctl")
    useKindCluster(mbg2Name)
    mbg2Pod, _       = getPodNameIp("mbg")
    mbg2Ip               = getKindIp("mbg2")
    gwctl2Pod, gwctl2Ip = getPodNameIp("gwctl")
    destkindIp=getKindIp(mbg2Name)

    
    # Add MBG Peer
    useKindCluster(mbg1Name)
    printHeader("Add MBG1 peer to MBG2")
    connectMbgs(mbg1Name, gwctl1Name, gwctl1Pod, mbg2Name, mbg2Ip, mbg2cPort)
    useKindCluster(mbg2Name)
    printHeader("Add MBG2 peer to MBG1")
    connectMbgs(mbg2Name, gwctl2Name, gwctl2Pod, mbg1Name, mbg1Ip, mbg1cPort)
    
    # Set service iperf3-client in MBG1
    setIperf3client(mbg1Name, gwctl1Name, srcSvc)
    
    # Set service iperf3-server in MBG2
    setIperf3Server(mbg2Name, gwctl2Name,destSvc)

    #Import destination service
    importService(mbg1Name, destSvc,destPort, mbg2Name)

    #Get services
    getService(mbg1Name, destSvc)
    #Add policy
    addPolicy(mbg1Name, gwctl1Name, command="create", action="allow", srcSvc=srcSvc,destSvc=destSvc, priority=0)
    #Testing
    printHeader("\n\nStart Iperf3 testing")
    useKindCluster(mbg2Name)
    waitPod("iperf3-server")
    #Test MBG1
    directTestIperf3(mbg1Name, srcSvc, destkindIp, kindDestPort)
    testIperf3Client(mbg1Name, srcSvc, destSvc,    destPort)
