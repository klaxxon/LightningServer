# LightningServer
This server is used for collecting LD-250 lightning data and providing a web interface for displaying the collected data.  Still a work in progress.  Hard coded to a lightning detector on /dev/ttyUSB0 and an OpenStreepMap tile server.<br/>
<br/>
![alt text](https://github.com/klaxxon/LightningServer/raw/master/Lightning.png "Logo Title Text 1")

# Server
This is a simple golang server which collects data from a Boltek LD-250 lighning detector, saves it to a Sqlite3 database, 
and provides webserver for client access to the data.

# Database
Sqlite3 database consists of a single table "strikes" with a timestamp, distance and heading.

The server will also pull the NOAA radar page every ten minutes so an animated playback of radar data can be overlayed onto the map.

# Client

The client serves up the single index.html with javascript providing some graphs and animated radar.

# Centering map

Simply change the clat and clng around line 40 in index.html to your devices location.


# Setting custom ranges

The default implementation is range rings of equal areas.  This provides a more accurate display in the graphs since lightning strikes are represented equally amongst the three ranges.  However, if you prefer fixed, linear distances from center, change the three values around line 40 in index.html and within apiGetStrikes in the ld250.go file to the ranges you prefer.

