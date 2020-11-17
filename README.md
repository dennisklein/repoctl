# MUNGE

The munge package.

MUNGE (**M**UNGE **U**id '**N**' **G**id **E**mporium) is an authentication service for creating and validating credentials.

Before starting the service as the first time a secret key has to been created.

For creating the key add store it to the file `/etc/munge/munge.key` use `/sbin/mungekey`. 
For example:
```
sudo -u munge %{_sbindir}/mungekey -v
```
Please read `man 8 mungekey` for more information.

In the second step you can start and enable the munge service.
```bash
systemctl start munge
systemctl enalbe munge
