from sim import *
ar=lambda a,b,s: [round(a+i*s,4) for i in range(int(round((b-a)/s)))]
# eta from sem-pilot overlap: FT ran 25.05, SO ran 39.87 (record-2, observed order A1,A2,A4,A3)
best=None
for eta in ar(0.60,1.001,0.01):
    v,d=simulate(2,"full",{"kind":"K"},["A1","A2","A4","A3"],eta=eta,dt=0.01)
    ft=[j for j in d if j.lab=="FT"][0]; so=[j for j in d if j.lab=="SO"][0]
    err=((ft.end-ft.start)/25.05-1)**2+((so.end-so.start)/39.87-1)**2
    if best is None or err<best[0]: best=(err,eta,ft.end-ft.start,so.end-so.start)
print("eta fit",best)
# beta from S2 phase 1: TC+TC ran ~25.0 together (m_ok = 8.5 GiB: TC alone 8.4 GiB runs at alone speed)
for beta in ar(1.0,3.01,0.05):
    v,d=simulate(2,"full",{"kind":"S","slots":2,"inherit":1},["A1","A2","A3","A4"],eta=best[1],beta=beta,dt=0.01)
    tc=[j.end for j in d if j.lab=="TC"]
    if max(tc)>=25.0: print("beta",round(beta,2),tc); break
