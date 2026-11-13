import os,sys
d=''
def load(f):
    rows={}
    for l in open(d+f):
        p=l.split()
        if len(p)==8: rows[p[1]]=(p[0],int(p[2]),int(p[3]),int(p[4]),int(p[5]),int(p[6]),float(p[7]))
    return rows
a=load(sys.argv[1]); b=load(sys.argv[2]); tot={}
for i in sorted(a, key=lambda k:a[k][0]):
    if i not in b: continue
    dep,rx0,tx0,bu0,bt0,n,t0=a[i]; _,rx1,tx1,bu1,bt1,_,t1=b[i]; dt=t1-t0
    rx=8*(rx1-rx0)/dt/1e9; tx=8*(tx1-tx0)/dt/1e9
    tot.setdefault(dep,[0,0]); tot[dep][0]+=rx; tot[dep][1]+=tx
    print(f"{dep} {i} rx={rx:6.2f} tx={tx:6.2f} Gbps cpu={100*(bu1-bu0)/max(bt1-bt0,1):5.1f}% ({n} vCPU, {dt:.0f}s)")
print("per-deployment sums (rx,tx Gbps):", {k:[round(x,1) for x in v] for k,v in tot.items()})
